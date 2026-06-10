package bootstrap

import "testing"

func TestApplyServiceDefaultsSetsRequiredGrpcFields(t *testing.T) {
	fields := map[string]any{}

	applyServiceDefaults(fields)

	if got := fields["max_request_size"]; got != int64(10*1024*1024) {
		t.Fatalf("max_request_size = %v, want 10485760", got)
	}
	if got := fields["request_timeout_seconds"]; got != int64(30) {
		t.Fatalf("request_timeout_seconds = %v, want 30", got)
	}
	if got := fields["shutdown_timeout_seconds"]; got != int64(30) {
		t.Fatalf("shutdown_timeout_seconds = %v, want 30", got)
	}
	listen, ok := fields["listen_address"].(map[string]any)
	if !ok {
		t.Fatalf("listen_address = %T, want map[string]any", fields["listen_address"])
	}
	if listen["host"] != "0.0.0.0" || listen["port"] != int64(8443) {
		t.Fatalf("listen_address = %#v, want 0.0.0.0:8443", listen)
	}
	if fields["logger"] == nil {
		t.Fatal("logger default was not set")
	}
}
