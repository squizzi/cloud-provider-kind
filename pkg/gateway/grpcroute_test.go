package gateway

import (
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func makeGRPCRuleWithFilters(filters ...gatewayv1.GRPCRouteFilter) gatewayv1.GRPCRouteRule {
	return gatewayv1.GRPCRouteRule{
		Filters: filters,
		BackendRefs: []gatewayv1.GRPCBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: "svc",
						Port: ptr.To(gatewayv1.PortNumber(80)),
					},
				},
			},
		},
	}
}

func makeGRPCRuleWithBackend(svcName string) gatewayv1.GRPCRouteRule {
	return gatewayv1.GRPCRouteRule{
		BackendRefs: []gatewayv1.GRPCBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(svcName),
						Port: ptr.To(gatewayv1.PortNumber(80)),
					},
				},
			},
		},
	}
}

func makeGRPCRuleWithMatch(match gatewayv1.GRPCRouteMatch) gatewayv1.GRPCRouteRule {
	return gatewayv1.GRPCRouteRule{
		Matches: []gatewayv1.GRPCRouteMatch{match},
		BackendRefs: []gatewayv1.GRPCBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: "svc",
						Port: ptr.To(gatewayv1.PortNumber(80)),
					},
				},
			},
		},
	}
}

func grpcHeaderModifierFilter() gatewayv1.GRPCRouteFilter {
	return gatewayv1.GRPCRouteFilter{
		Type: gatewayv1.GRPCRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set:    []gatewayv1.HTTPHeader{{Name: "X-Header-Set", Value: "set-overwritten-value"}},
			Add:    []gatewayv1.HTTPHeader{{Name: "X-Header-Add", Value: "add-value"}},
			Remove: []string{"X-Header-Remove"},
		},
	}
}

func grpcRequestMirrorFilter() gatewayv1.GRPCRouteFilter {
	return gatewayv1.GRPCRouteFilter{
		Type: gatewayv1.GRPCRouteFilterRequestMirror,
		RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
			BackendRef: gatewayv1.BackendObjectReference{
				Name: "svc",
				Port: ptr.To(gatewayv1.PortNumber(80)),
			},
		},
	}
}

func grpcResponseHeaderFilter() gatewayv1.GRPCRouteFilter {
	return gatewayv1.GRPCRouteFilter{
		Type:                   gatewayv1.GRPCRouteFilterResponseHeaderModifier,
		ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{},
	}
}

func TestTranslateGRPCRouteToEnvoyRoutes(t *testing.T) {
	svc := makeService("default", "svc", 80)
	svcLister := newMockServiceLister(svc)
	noGrants := newFakeReferenceGrantLister(nil, nil)

	baseRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-route",
			Namespace:  "default",
			Generation: 1,
		},
	}

	tests := []struct {
		name                  string
		rules                 []gatewayv1.GRPCRouteRule
		wantRoutes            int
		wantAcceptedFalse     bool
		wantResolvedRefsFalse bool
		wantPartiallyInvalid  bool
	}{
		{
			name: "exact method match with service and method",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithMatch(gatewayv1.GRPCRouteMatch{
					Method: &gatewayv1.GRPCMethodMatch{
						Type:    ptr.To(gatewayv1.GRPCMethodMatchExact),
						Service: ptr.To("example.Echo"),
						Method:  ptr.To("Ping"),
					},
				}),
			},
			wantRoutes: 1,
		},
		{
			name: "request header modifier is a supported core filter",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcHeaderModifierFilter()),
			},
			wantRoutes: 1,
		},
		{
			name: "single rule with unsupported filter - fully invalid",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcRequestMirrorFilter()),
			},
			wantAcceptedFalse: true,
		},
		{
			name: "first rule unsupported filter, second rule supported - partially invalid",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcResponseHeaderFilter()),
				makeGRPCRuleWithFilters(grpcHeaderModifierFilter()),
			},
			wantRoutes:           1,
			wantPartiallyInvalid: true,
		},
		{
			name: "backend not found - ResolvedRefs=False",
			rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithBackend("missing-svc"),
			},
			wantRoutes:            1,
			wantResolvedRefsFalse: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := baseRoute.DeepCopy()
			route.Spec.Rules = tt.rules

			routes, _, notAccepted, resolvedRefsFailure, partiallyInvalid := translateGRPCRouteToEnvoyRoutes(route, svcLister, noGrants)

			if len(routes) != tt.wantRoutes {
				t.Errorf("got %d routes, want %d", len(routes), tt.wantRoutes)
			}

			if tt.wantAcceptedFalse {
				if notAccepted == nil {
					t.Fatalf("Accepted condition missing, want Accepted=False")
				}
				if notAccepted.Status != metav1.ConditionFalse {
					t.Errorf("Accepted.Status = %q, want False", notAccepted.Status)
				}
				if resolvedRefsFailure != nil {
					t.Errorf("unexpected ResolvedRefs condition when route is fully rejected")
				}
				if partiallyInvalid != nil {
					t.Errorf("unexpected PartiallyInvalid condition when route is fully rejected")
				}
			} else if notAccepted != nil {
				t.Errorf("unexpected Accepted=False condition")
			}

			if tt.wantResolvedRefsFalse {
				if resolvedRefsFailure == nil {
					t.Fatalf("ResolvedRefs condition is nil, want ResolvedRefs=False")
				}
			} else if resolvedRefsFailure != nil {
				t.Errorf("unexpected ResolvedRefs condition")
			}

			if tt.wantPartiallyInvalid {
				if partiallyInvalid == nil {
					t.Fatalf("PartiallyInvalid condition missing, want PartiallyInvalid=True")
				}
				if !strings.HasPrefix(partiallyInvalid.Message, "Dropped Rule") {
					t.Errorf("PartiallyInvalid.Message = %q, want prefix \"Dropped Rule\"", partiallyInvalid.Message)
				}
			} else if partiallyInvalid != nil {
				t.Errorf("unexpected PartiallyInvalid condition")
			}
		})
	}
}

func TestGRPCRouteRequestHeaderModifierApplied(t *testing.T) {
	svc := makeService("default", "svc", 80)
	svcLister := newMockServiceLister(svc)
	noGrants := newFakeReferenceGrantLister(nil, nil)

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "hdr", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{
				makeGRPCRuleWithFilters(grpcHeaderModifierFilter()),
			},
		},
	}

	routes, _, notAccepted, _, _ := translateGRPCRouteToEnvoyRoutes(route, svcLister, noGrants)
	if notAccepted != nil {
		t.Fatalf("unexpected Accepted=False: %v", notAccepted)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}

	got := routes[0]
	if len(got.RequestHeadersToAdd) != 2 {
		t.Fatalf("got %d headers to add, want 2", len(got.RequestHeadersToAdd))
	}

	setHeader := got.RequestHeadersToAdd[0]
	if setHeader.Header.GetKey() != "X-Header-Set" || setHeader.Header.GetValue() != "set-overwritten-value" {
		t.Errorf("set header = %s=%s, want X-Header-Set=set-overwritten-value", setHeader.Header.GetKey(), setHeader.Header.GetValue())
	}
	if setHeader.AppendAction != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
		t.Errorf("set AppendAction = %v, want OVERWRITE_IF_EXISTS_OR_ADD", setHeader.AppendAction)
	}

	addHeader := got.RequestHeadersToAdd[1]
	if addHeader.Header.GetKey() != "X-Header-Add" || addHeader.Header.GetValue() != "add-value" {
		t.Errorf("add header = %s=%s, want X-Header-Add=add-value", addHeader.Header.GetKey(), addHeader.Header.GetValue())
	}
	if addHeader.AppendAction != corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD {
		t.Errorf("add AppendAction = %v, want APPEND_IF_EXISTS_OR_ADD", addHeader.AppendAction)
	}

	if len(got.RequestHeadersToRemove) != 1 || got.RequestHeadersToRemove[0] != "X-Header-Remove" {
		t.Errorf("RequestHeadersToRemove = %v, want [X-Header-Remove]", got.RequestHeadersToRemove)
	}
}

func TestTranslateGRPCRouteMatchExactServiceAndMethod(t *testing.T) {
	match := gatewayv1.GRPCRouteMatch{
		Method: &gatewayv1.GRPCMethodMatch{
			Type:    ptr.To(gatewayv1.GRPCMethodMatchExact),
			Service: ptr.To("example.Echo"),
			Method:  ptr.To("Ping"),
		},
	}
	routeMatch, dropReason := translateGRPCRouteMatch(match)
	if dropReason != nil {
		t.Fatalf("unexpected drop reason: %s", *dropReason)
	}
	if routeMatch.GetPath() != "/example.Echo/Ping" {
		t.Errorf("path = %q, want %q", routeMatch.GetPath(), "/example.Echo/Ping")
	}
}
