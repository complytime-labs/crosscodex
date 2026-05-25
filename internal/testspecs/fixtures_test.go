package testspecs

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestTenantFixture_ValidTenants(t *testing.T) {
	// Test that StandardTenantContexts contains valid tenant fixtures
	validFixture := StandardTenantContexts["valid-tenant"]

	if !validFixture.Valid {
		t.Errorf("expected valid-tenant fixture to be Valid=true, got %v", validFixture.Valid)
	}

	if validFixture.TenantID == "" {
		t.Error("expected valid-tenant fixture to have non-empty TenantID")
	}

	// Validate the tenant ID using pkg/tenant
	if err := tenant.ValidateTenantID(validFixture.TenantID); err != nil {
		t.Errorf("valid-tenant fixture TenantID %q failed pkg/tenant validation: %v", validFixture.TenantID, err)
	}
}

func TestTenantFixture_InvalidTenants(t *testing.T) {
	// Test that invalid tenant fixtures properly fail validation
	invalidFixture := StandardTenantContexts["invalid-chars"]

	if invalidFixture.Valid {
		t.Errorf("expected invalid-chars fixture to be Valid=false, got %v", invalidFixture.Valid)
	}

	if invalidFixture.ErrorType == "" {
		t.Error("expected invalid-chars fixture to have non-empty ErrorType")
	}

	// Validate that it actually fails pkg/tenant validation
	if err := tenant.ValidateTenantID(invalidFixture.TenantID); err == nil {
		t.Errorf("invalid-chars fixture TenantID %q should fail pkg/tenant validation", invalidFixture.TenantID)
	}
}

func TestTenantFixture_MinMaxLength(t *testing.T) {
	// Test minimum length tenant
	minFixture := StandardTenantContexts["min-length"]
	if !minFixture.Valid {
		t.Errorf("expected min-length fixture to be Valid=true, got %v", minFixture.Valid)
	}

	if len(minFixture.TenantID) != 3 {
		t.Errorf("expected min-length fixture to have TenantID of length 3, got %d", len(minFixture.TenantID))
	}

	// Test maximum length tenant
	maxFixture := StandardTenantContexts["max-length"]
	if !maxFixture.Valid {
		t.Errorf("expected max-length fixture to be Valid=true, got %v", maxFixture.Valid)
	}

	if len(maxFixture.TenantID) != 64 {
		t.Errorf("expected max-length fixture to have TenantID of length 64, got %d", len(maxFixture.TenantID))
	}
}

func TestTenantFixture_TooShort(t *testing.T) {
	shortFixture := StandardTenantContexts["too-short"]

	if shortFixture.Valid {
		t.Errorf("expected too-short fixture to be Valid=false, got %v", shortFixture.Valid)
	}

	if len(shortFixture.TenantID) >= 3 {
		t.Errorf("expected too-short fixture to have TenantID shorter than 3 chars, got %d", len(shortFixture.TenantID))
	}
}

func TestTenantFixture_TooLong(t *testing.T) {
	longFixture := StandardTenantContexts["too-long"]

	if longFixture.Valid {
		t.Errorf("expected too-long fixture to be Valid=false, got %v", longFixture.Valid)
	}

	if len(longFixture.TenantID) <= 64 {
		t.Errorf("expected too-long fixture to have TenantID longer than 64 chars, got %d", len(longFixture.TenantID))
	}
}

func TestTenantFixture_EmptyTenant(t *testing.T) {
	emptyFixture := StandardTenantContexts["empty"]

	if emptyFixture.Valid {
		t.Errorf("expected empty fixture to be Valid=false, got %v", emptyFixture.Valid)
	}

	if emptyFixture.TenantID != "" {
		t.Errorf("expected empty fixture to have empty TenantID, got %q", emptyFixture.TenantID)
	}
}

func TestCreateTenantContext(t *testing.T) {
	tenantID := "acme-corp"
	ctx := CreateTenantContext(tenantID)

	extractedTenantID, err := tenant.FromContext(ctx)
	if err != nil {
		t.Errorf("failed to extract tenant ID from context: %v", err)
	}

	if extractedTenantID != tenantID {
		t.Errorf("expected tenant ID %q, got %q", tenantID, extractedTenantID)
	}
}

func TestCreateTimeoutContext(t *testing.T) {
	timeout := 100 * time.Millisecond
	ctx, cancel := CreateTimeoutContext(timeout)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Error("expected timeout context to have deadline")
	}

	if time.Until(deadline) > timeout {
		t.Errorf("expected deadline within %v, but it's %v away", timeout, time.Until(deadline))
	}
}

func TestCreateCancelableContext(t *testing.T) {
	ctx, cancel := CreateCancelableContext()

	select {
	case <-ctx.Done():
		t.Error("context should not be cancelled initially")
	default:
		// good, context is not cancelled
	}

	cancel()

	select {
	case <-ctx.Done():
		// good, context is now cancelled
	case <-time.After(10 * time.Millisecond):
		t.Error("context should be cancelled after calling cancel()")
	}
}

func TestConfigPathFixture_XDGPrecedence(t *testing.T) {
	// Test XDG configuration path precedence rules
	xdgFixture := StandardConfigPaths["xdg-precedence"]

	if len(xdgFixture.Paths) == 0 {
		t.Error("expected xdg-precedence fixture to have at least one path")
	}

	if xdgFixture.Priority != "user-config" {
		t.Errorf("expected xdg-precedence fixture to have Priority=user-config, got %q", xdgFixture.Priority)
	}

	// Should contain XDG config paths
	hasXDGConfig := false
	for _, path := range xdgFixture.Paths {
		if strings.Contains(path, "config") || strings.Contains(path, "XDG_CONFIG_HOME") {
			hasXDGConfig = true
			break
		}
	}
	if !hasXDGConfig {
		t.Error("expected xdg-precedence fixture to contain XDG config paths")
	}
}

func TestConfigPathFixture_SystemPaths(t *testing.T) {
	// Test system-wide configuration paths
	systemFixture := StandardConfigPaths["system-paths"]

	if systemFixture.Priority != "system-wide" {
		t.Errorf("expected system-paths fixture to have Priority=system-wide, got %q", systemFixture.Priority)
	}

	// Should contain /etc or similar system paths
	hasSystemPath := false
	for _, path := range systemFixture.Paths {
		if strings.HasPrefix(path, "/etc") || strings.HasPrefix(path, "/usr") {
			hasSystemPath = true
			break
		}
	}
	if !hasSystemPath {
		t.Error("expected system-paths fixture to contain system paths like /etc or /usr")
	}
}

func TestConfigPathFixture_LocalFiles(t *testing.T) {
	// Test local configuration files
	localFixture := StandardConfigPaths["local-files"]

	if localFixture.Priority != "local" {
		t.Errorf("expected local-files fixture to have Priority=local, got %q", localFixture.Priority)
	}

	// Should contain relative paths
	hasLocalPath := false
	for _, path := range localFixture.Paths {
		if !filepath.IsAbs(path) {
			hasLocalPath = true
			break
		}
	}
	if !hasLocalPath {
		t.Error("expected local-files fixture to contain relative paths")
	}
}

func TestErrorCondition_NetworkTimeout(t *testing.T) {
	// Test network timeout error condition
	timeoutError := StandardErrors["network-timeout"]

	if !timeoutError.Retryable {
		t.Errorf("expected network-timeout to be Retryable=true, got %v", timeoutError.Retryable)
	}

	if timeoutError.Category != "network" {
		t.Errorf("expected network-timeout to have Category=network, got %q", timeoutError.Category)
	}

	if timeoutError.Message == "" {
		t.Error("expected network-timeout to have non-empty Message")
	}
}

func TestErrorCondition_PermissionDenied(t *testing.T) {
	// Test permission denied error condition
	permError := StandardErrors["permission-denied"]

	if permError.Retryable {
		t.Errorf("expected permission-denied to be Retryable=false, got %v", permError.Retryable)
	}

	if permError.Category != "authorization" {
		t.Errorf("expected permission-denied to have Category=authorization, got %q", permError.Category)
	}
}

func TestErrorCondition_InvalidInput(t *testing.T) {
	// Test invalid input error condition
	inputError := StandardErrors["invalid-input"]

	if inputError.Retryable {
		t.Errorf("expected invalid-input to be Retryable=false, got %v", inputError.Retryable)
	}

	if inputError.Category != "validation" {
		t.Errorf("expected invalid-input to have Category=validation, got %q", inputError.Category)
	}
}

func TestErrorCondition_NotFound(t *testing.T) {
	// Test not found error condition
	notFoundError := StandardErrors["not-found"]

	if notFoundError.Retryable {
		t.Errorf("expected not-found to be Retryable=false, got %v", notFoundError.Retryable)
	}

	if notFoundError.Category != "client" {
		t.Errorf("expected not-found to have Category=client, got %q", notFoundError.Category)
	}
}
