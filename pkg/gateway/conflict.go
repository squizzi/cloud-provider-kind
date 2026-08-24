package gateway

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const httpGRPCHostnameConflictMessage = "hostname intersects a route of the other type on the same listener; the other route was preferred"

func applyHTTPGRPCHostnameUniqueness(
	gateway *gatewayv1.Gateway,
	httpByListener map[gatewayv1.SectionName][]*gatewayv1.HTTPRoute,
	grpcByListener map[gatewayv1.SectionName][]*gatewayv1.GRPCRoute,
	httpStatuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
	grpcStatuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
) {
	for _, listener := range gateway.Spec.Listeners {
		httpRoutes := httpByListener[listener.Name]
		grpcRoutes := grpcByListener[listener.Name]
		if len(httpRoutes) == 0 || len(grpcRoutes) == 0 {
			continue
		}

		httpReject := make([]bool, len(httpRoutes))
		grpcReject := make([]bool, len(grpcRoutes))
		for i, httpRoute := range httpRoutes {
			for j, grpcRoute := range grpcRoutes {
				if !routeHostnamesOverlap(listener, httpRoute.Spec.Hostnames, grpcRoute.Spec.Hostnames) {
					continue
				}
				if routePrecedenceWins(httpRoute.ObjectMeta, grpcRoute.ObjectMeta) {
					grpcReject[j] = true
				} else {
					httpReject[i] = true
				}
			}
		}

		httpByListener[listener.Name] = filterHTTPRoutes(httpRoutes, httpReject)
		grpcByListener[listener.Name] = filterGRPCRoutes(grpcRoutes, grpcReject)
	}

	markConflictingHTTPParents(gateway, httpByListener, httpStatuses)
	markConflictingGRPCParents(gateway, grpcByListener, grpcStatuses)
}

func filterHTTPRoutes(routes []*gatewayv1.HTTPRoute, reject []bool) []*gatewayv1.HTTPRoute {
	var kept []*gatewayv1.HTTPRoute
	for i, route := range routes {
		if !reject[i] {
			kept = append(kept, route)
		}
	}
	return kept
}

func filterGRPCRoutes(routes []*gatewayv1.GRPCRoute, reject []bool) []*gatewayv1.GRPCRoute {
	var kept []*gatewayv1.GRPCRoute
	for i, route := range routes {
		if !reject[i] {
			kept = append(kept, route)
		}
	}
	return kept
}

func markConflictingHTTPParents(
	gateway *gatewayv1.Gateway,
	remainingByListener map[gatewayv1.SectionName][]*gatewayv1.HTTPRoute,
	statuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
) {
	remaining := map[types.NamespacedName]map[gatewayv1.SectionName]struct{}{}
	for listenerName, routes := range remainingByListener {
		for _, route := range routes {
			key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
			if remaining[key] == nil {
				remaining[key] = map[gatewayv1.SectionName]struct{}{}
			}
			remaining[key][listenerName] = struct{}{}
		}
	}
	rejectParentsWithoutListener(gateway, remaining, statuses)
}

func markConflictingGRPCParents(
	gateway *gatewayv1.Gateway,
	remainingByListener map[gatewayv1.SectionName][]*gatewayv1.GRPCRoute,
	statuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
) {
	remaining := map[types.NamespacedName]map[gatewayv1.SectionName]struct{}{}
	for listenerName, routes := range remainingByListener {
		for _, route := range routes {
			key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
			if remaining[key] == nil {
				remaining[key] = map[gatewayv1.SectionName]struct{}{}
			}
			remaining[key][listenerName] = struct{}{}
		}
	}
	rejectParentsWithoutListener(gateway, remaining, statuses)
}

func rejectParentsWithoutListener(
	gateway *gatewayv1.Gateway,
	remaining map[types.NamespacedName]map[gatewayv1.SectionName]struct{},
	statuses map[types.NamespacedName][]gatewayv1.RouteParentStatus,
) {
	for key, parentStatuses := range statuses {
		for i := range parentStatuses {
			if !meta.IsStatusConditionTrue(parentStatuses[i].Conditions, string(gatewayv1.RouteConditionAccepted)) {
				continue
			}
			if parentStillHasListener(gateway, parentStatuses[i].ParentRef, remaining[key]) {
				continue
			}
			meta.SetStatusCondition(&parentStatuses[i].Conditions, metav1.Condition{
				Type:    string(gatewayv1.RouteConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayv1.RouteReasonUnsupportedValue),
				Message: httpGRPCHostnameConflictMessage,
			})
		}
		statuses[key] = parentStatuses
	}
}

func parentStillHasListener(gateway *gatewayv1.Gateway, parentRef gatewayv1.ParentReference, remainingListeners map[gatewayv1.SectionName]struct{}) bool {
	if remainingListeners == nil {
		return false
	}
	refNamespace := gateway.Namespace
	if parentRef.Namespace != nil {
		refNamespace = string(*parentRef.Namespace)
	}
	if parentRef.Name != gatewayv1.ObjectName(gateway.Name) || refNamespace != gateway.Namespace {
		return false
	}
	for _, listener := range gateway.Spec.Listeners {
		sectionNameMatches := parentRef.SectionName == nil || *parentRef.SectionName == listener.Name
		portMatches := parentRef.Port == nil || *parentRef.Port == listener.Port
		if !sectionNameMatches || !portMatches {
			continue
		}
		if _, ok := remainingListeners[listener.Name]; ok {
			return true
		}
	}
	return false
}
