package gateway

import (
	"errors"
	"fmt"
	"strings"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1listers "k8s.io/client-go/listers/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewaylistersv1 "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

func translateGRPCRouteToEnvoyRoutes(
	grpcRoute *gatewayv1.GRPCRoute,
	serviceLister corev1listers.ServiceLister,
	referenceGrantLister gatewaylistersv1.ReferenceGrantLister,
) (
	envoyRoutes []*routev3.Route,
	allValidBackendRefs []gatewayv1.BackendRef,
	notAccepted *metav1.Condition,
	resolvedRefsFailure *metav1.Condition,
	partiallyInvalid *metav1.Condition,
) {
	var droppedRuleMessages []string

	for ruleIndex, rule := range grpcRoute.Spec.Rules {
		if unsupportedType, found := findUnsupportedGRPCFilter(rule.Filters); found {
			droppedRuleMessages = append(droppedRuleMessages,
				fmt.Sprintf("rule[%d] has unsupported filter type %q", ruleIndex, unsupportedType))
			continue
		}

		buildRoutesForRule := func(match gatewayv1.GRPCRouteMatch, matchIndex int) {
			routeMatch, dropReason := translateGRPCRouteMatch(match)
			if dropReason != nil {
				droppedRuleMessages = append(droppedRuleMessages,
					fmt.Sprintf("rule[%d] match[%d]: %s", ruleIndex, matchIndex, *dropReason))
				return
			}

			envoyRoute := &routev3.Route{
				Name:  fmt.Sprintf("%s-%s-rule%d-match%d", grpcRoute.Namespace, grpcRoute.Name, ruleIndex, matchIndex),
				Match: routeMatch,
			}

			routeAction, validBackends, err := buildGRPCRouteAction(
				grpcRoute.Namespace,
				rule.BackendRefs,
				serviceLister,
				referenceGrantLister,
			)
			var controllerErr *ControllerError
			var noBackendsErr *noEffectiveBackendsError
			switch {
			case errors.As(err, &noBackendsErr):
				envoyRoute.Action = &routev3.Route_DirectResponse{
					DirectResponse: &routev3.DirectResponseAction{Status: 500},
				}
			case errors.As(err, &controllerErr):
				cond := createNotResolvedCondition(gatewayv1.RouteConditionReason(controllerErr.Reason), controllerErr.Message, grpcRoute.Generation)
				resolvedRefsFailure = &cond
				envoyRoute.Action = &routev3.Route_DirectResponse{
					DirectResponse: &routev3.DirectResponseAction{Status: 500},
				}
			default:
				allValidBackendRefs = append(allValidBackendRefs, validBackends...)
				envoyRoute.Action = &routev3.Route_Route{
					Route: routeAction,
				}
			}
			envoyRoutes = append(envoyRoutes, envoyRoute)
		}

		if len(rule.Matches) == 0 {
			buildRoutesForRule(gatewayv1.GRPCRouteMatch{}, 0)
		} else {
			for matchIndex, match := range rule.Matches {
				buildRoutesForRule(match, matchIndex)
			}
		}
	}

	if len(droppedRuleMessages) > 0 && len(envoyRoutes) == 0 {
		msg := fmt.Sprintf("no rules could be translated: %s", strings.Join(droppedRuleMessages, "; "))
		cond := createNotAcceptedCondition(gatewayv1.RouteReasonUnsupportedValue, msg, grpcRoute.Generation)
		notAccepted = &cond
		return nil, nil, notAccepted, nil, nil
	}

	if len(droppedRuleMessages) > 0 {
		msg := fmt.Sprintf("Dropped Rule(s): %s", strings.Join(droppedRuleMessages, "; "))
		cond := createPartiallyInvalidCondition(msg, grpcRoute.Generation)
		partiallyInvalid = &cond
	}
	return envoyRoutes, allValidBackendRefs, nil, resolvedRefsFailure, partiallyInvalid
}

func findUnsupportedGRPCFilter(filters []gatewayv1.GRPCRouteFilter) (gatewayv1.GRPCRouteFilterType, bool) {
	for _, filter := range filters {
		switch filter.Type {
		case gatewayv1.GRPCRouteFilterRequestHeaderModifier:
		default:
			return filter.Type, true
		}
	}
	return "", false
}

func buildGRPCRouteAction(namespace string, backendRefs []gatewayv1.GRPCBackendRef, serviceLister corev1listers.ServiceLister, referenceGrantLister gatewaylistersv1.ReferenceGrantLister) (*routev3.RouteAction, []gatewayv1.BackendRef, error) {
	weightedClusters := &routev3.WeightedCluster{}
	var validBackendRefs []gatewayv1.BackendRef

	for _, grpcBackendRef := range backendRefs {
		backendRef := grpcBackendRef.BackendRef

		ns := namespace
		if backendRef.Namespace != nil {
			ns = string(*backendRef.Namespace)
		}

		if ns != namespace {
			from := gatewayv1.ReferenceGrantFrom{
				Group:     gatewayv1.GroupName,
				Kind:      "GRPCRoute",
				Namespace: gatewayv1.Namespace(namespace),
			}
			to := gatewayv1.ReferenceGrantTo{
				Group: "",
				Kind:  "Service",
				Name:  &backendRef.Name,
			}

			if !isCrossNamespaceRefAllowed(from, to, ns, referenceGrantLister) {
				return nil, nil, &ControllerError{
					Reason:  string(gatewayv1.RouteReasonRefNotPermitted),
					Message: "permission error",
				}
			}
		}

		if _, err := serviceLister.Services(ns).Get(string(backendRef.Name)); err != nil {
			return nil, nil, &ControllerError{
				Reason:  string(gatewayv1.RouteReasonBackendNotFound),
				Message: "backend not found",
			}
		}
		clusterName, err := backendRefToClusterName(namespace, backendRef)
		if err != nil {
			return nil, nil, err
		}

		weight := int32(1)
		if grpcBackendRef.Weight != nil {
			weight = *grpcBackendRef.Weight
		}
		if weight == 0 {
			continue
		}
		validBackendRefs = append(validBackendRefs, backendRef)
		weightedClusters.Clusters = append(weightedClusters.Clusters, &routev3.WeightedCluster_ClusterWeight{
			Name:   clusterName,
			Weight: &wrapperspb.UInt32Value{Value: uint32(weight)},
		})
	}

	if len(weightedClusters.Clusters) == 0 {
		return nil, nil, &noEffectiveBackendsError{}
	}

	var action *routev3.RouteAction
	if len(weightedClusters.Clusters) == 1 {
		action = &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: weightedClusters.Clusters[0].Name}}
	} else {
		action = &routev3.RouteAction{ClusterSpecifier: &routev3.RouteAction_WeightedClusters{WeightedClusters: weightedClusters}}
	}

	return action, validBackendRefs, nil
}

// translateGRPCRouteMatch translates a GRPCRouteMatch to an Envoy RouteMatch.
// gRPC service/method maps to HTTP/2 path: /package.Service/Method
func translateGRPCRouteMatch(match gatewayv1.GRPCRouteMatch) (*routev3.RouteMatch, *string) {
	routeMatch := &routev3.RouteMatch{}

	if match.Method != nil {
		service := ""
		if match.Method.Service != nil {
			service = *match.Method.Service
		}
		method := ""
		if match.Method.Method != nil {
			method = *match.Method.Method
		}

		matchType := gatewayv1.GRPCMethodMatchExact
		if match.Method.Type != nil {
			matchType = *match.Method.Type
		}

		switch matchType {
		case gatewayv1.GRPCMethodMatchExact:
			if service != "" && method != "" {
				routeMatch.PathSpecifier = &routev3.RouteMatch_Path{Path: "/" + service + "/" + method}
			} else if service != "" {
				routeMatch.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/" + service + "/"}
			} else if method != "" {
				routeMatch.PathSpecifier = &routev3.RouteMatch_SafeRegex{
					SafeRegex: &matcherv3.RegexMatcher{
						EngineType: &matcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &matcherv3.RegexMatcher_GoogleRE2{}},
						Regex:      "/[^/]+/" + method,
					},
				}
			} else {
				routeMatch.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/"}
			}
		case gatewayv1.GRPCMethodMatchRegularExpression:
			var pattern string
			if service != "" && method != "" {
				pattern = "/" + service + "/" + method
			} else if service != "" {
				pattern = "/" + service + "/.*"
			} else if method != "" {
				pattern = "/[^/]+/" + method
			} else {
				pattern = "/.*"
			}
			routeMatch.PathSpecifier = &routev3.RouteMatch_SafeRegex{
				SafeRegex: &matcherv3.RegexMatcher{
					EngineType: &matcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &matcherv3.RegexMatcher_GoogleRE2{}},
					Regex:      pattern,
				},
			}
		default:
			msg := fmt.Sprintf("unsupported gRPC method match type: %s", matchType)
			return nil, &msg
		}
	} else {
		routeMatch.PathSpecifier = &routev3.RouteMatch_Prefix{Prefix: "/"}
	}

	for _, headerMatch := range match.Headers {
		headerMatcher := &routev3.HeaderMatcher{
			Name: string(headerMatch.Name),
		}
		matchType := gatewayv1.GRPCHeaderMatchExact
		if headerMatch.Type != nil {
			matchType = *headerMatch.Type
		}

		switch matchType {
		case gatewayv1.GRPCHeaderMatchExact:
			headerMatcher.HeaderMatchSpecifier = &routev3.HeaderMatcher_StringMatch{
				StringMatch: &matcherv3.StringMatcher{
					MatchPattern: &matcherv3.StringMatcher_Exact{Exact: headerMatch.Value},
				},
			}
		case gatewayv1.GRPCHeaderMatchRegularExpression:
			headerMatcher.HeaderMatchSpecifier = &routev3.HeaderMatcher_SafeRegexMatch{
				SafeRegexMatch: &matcherv3.RegexMatcher{
					EngineType: &matcherv3.RegexMatcher_GoogleRe2{GoogleRe2: &matcherv3.RegexMatcher_GoogleRE2{}},
					Regex:      headerMatch.Value,
				},
			}
		default:
			msg := fmt.Sprintf("unsupported header match type: %s", matchType)
			return nil, &msg
		}
		routeMatch.Headers = append(routeMatch.Headers, headerMatcher)
	}

	return routeMatch, nil
}
