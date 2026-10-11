package scope

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestCheckApprovalRoutingLimits(t *testing.T) {
	escalated := route("pay", `{"tools":["pay.*","wire"]}`, `"escalation":[{"extension_seconds":7200}]`)
	value := []byte(routes(escalated, route("default", `{}`)))
	if err := CheckApprovalRoutingLimits(value, domain.ApprovalRoutingLimits{}); err != nil {
		t.Fatalf("the structural bounds accept what decodes: %v", err)
	}
	fits := domain.ApprovalRoutingLimits{MaxRoutesPerBinding: 2, MaxToolPatterns: 2, MinDurationSeconds: 3600, MaxDurationSeconds: 7200, MaxWindowSeconds: 10800}
	if err := CheckApprovalRoutingLimits(value, fits); err != nil {
		t.Fatalf("limits the value meets exactly: %v", err)
	}
	for name, limits := range map[string]domain.ApprovalRoutingLimits{
		"routes per binding": {MaxRoutesPerBinding: 1},
		"tool patterns":      {MaxToolPatterns: 1},
		"raised floor":       {MinDurationSeconds: 3601},
		"lowered ceiling":    {MaxDurationSeconds: 3599},
		"extension":          {MaxDurationSeconds: 7199, MinDurationSeconds: 60},
		"window":             {MaxWindowSeconds: 10799},
	} {
		if err := CheckApprovalRoutingLimits(value, limits); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
	if err := CheckApprovalRoutingLimits(value, domain.ApprovalRoutingLimits{MaxRoutes: -1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid limits: want ErrValidation, got %v", err)
	}
	many := make([]string, 0, maxRoutesPerBinding+1)
	for index := range maxRoutesPerBinding + 1 {
		many = append(many, route(fmt.Sprintf("r%d", index), fmt.Sprintf(`{"tools":["t%d"]}`, index)))
	}
	err := CheckApprovalRoutingLimits([]byte(routes(many...)), domain.ApprovalRoutingLimits{})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "the limit is 32") {
		t.Fatalf("the absolute ceiling still holds: %v", err)
	}
}
