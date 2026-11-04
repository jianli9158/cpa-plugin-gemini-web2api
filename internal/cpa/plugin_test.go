package cpa_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mag1cFall/Gemini-Web2API/internal/cpa"
	"github.com/Mag1cFall/Gemini-Web2API/internal/pluginabi"
	"github.com/Mag1cFall/Gemini-Web2API/internal/pluginapi"
)

func TestPluginLifecycleAndCapabilities(t *testing.T) {
	tmpDir := t.TempDir()
	var hostCalls []string
	p := cpa.NewPlugin(func(method string, payload []byte) ([]byte, error) {
		hostCalls = append(hostCalls, method)
		return json.Marshal(pluginabi.Envelope{
			OK:     true,
			Result: json.RawMessage(`{}`),
		})
	})
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, nil)
	}()

	cfgYAML := []byte("data_dir: " + tmpDir + "\nauto_start: false\n")
	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    cfgYAML,
		"schema_version": pluginabi.SchemaVersion,
	})

	// 1. plugin.register
	regRes, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq)
	if err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}
	regJSON, _ := json.Marshal(regRes)
	if !strings.Contains(string(regJSON), `"Name":"gemini-web2api"`) {
		t.Fatalf("unexpected registration metadata: %s", string(regJSON))
	}
	if !strings.Contains(string(regJSON), `"executor":true`) || !strings.Contains(string(regJSON), `"auth_provider":true`) {
		t.Fatalf("missing expected capabilities: %s", string(regJSON))
	}

	// 2. auth.identifier & auth.parse
	authIDRes, err := p.HandleMethod(pluginabi.MethodAuthIdentifier, nil)
	if err != nil {
		t.Fatalf("auth.identifier failed: %v", err)
	}
	authIDJSON, _ := json.Marshal(authIDRes)
	if !strings.Contains(string(authIDJSON), `"gemini-web2api"`) {
		t.Fatalf("unexpected auth.identifier: %s", string(authIDJSON))
	}

	// Unrelated provider should return Handled: false
	unrelatedParseReq, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: "openai",
		FileName: "openai-key.json",
		RawJSON:  []byte(`{"type":"openai"}`),
	})
	unrelatedRes, err := p.HandleMethod(pluginabi.MethodAuthParse, unrelatedParseReq)
	if err != nil {
		t.Fatalf("auth.parse unrelated failed: %v", err)
	}
	if unrelatedRes.(pluginapi.AuthParseResponse).Handled {
		t.Fatalf("expected Handled=false for unrelated provider")
	}

	// Matching gemini-web2api auth file
	matchingParseReq, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: "gemini-web2api",
		FileName: "gemini-web2api-user@example.com.json",
		RawJSON:  []byte(`{"type":"gemini-web2api","account_id":"user@example.com","tier":"PRO"}`),
	})
	matchingRes, err := p.HandleMethod(pluginabi.MethodAuthParse, matchingParseReq)
	if err != nil {
		t.Fatalf("auth.parse matching failed: %v", err)
	}
	parsedAuth := matchingRes.(pluginapi.AuthParseResponse)
	if !parsedAuth.Handled || parsedAuth.Auth.Provider != "gemini-web2api" || parsedAuth.Auth.Attributes["account_id"] != "user@example.com" {
		t.Fatalf("unexpected parsed auth: %+v", parsedAuth)
	}

	// 3. model.static & model.for_auth & model.route
	staticRes, err := p.HandleMethod(pluginabi.MethodModelStatic, []byte(`{}`))
	if err != nil {
		t.Fatalf("model.static failed: %v", err)
	}
	modelResp := staticRes.(pluginapi.ModelResponse)
	if modelResp.Provider != "gemini-web2api" || len(modelResp.Models) == 0 {
		t.Fatalf("expected non-empty models from model.static, got: %+v", modelResp)
	}
	hasPrefixed := false
	hasUnprefixed := false
	for _, m := range modelResp.Models {
		if m.ID == "gemini-2.5-pro" || m.ID == "gemini-3.0-pro" || m.ID == "gemini-2.5-flash" {
			hasUnprefixed = true
		}
		if strings.HasPrefix(m.ID, "gemini-web/") {
			hasPrefixed = true
		}
	}
	if !hasUnprefixed || hasPrefixed {
		t.Fatalf("expected unprefixed gemini models without duplicate gemini-web/* prefix in catalog")
	}

	routeReq, _ := json.Marshal(pluginapi.ModelRouteRequest{
		RequestedModel: "gemini-web/gemini-3.0-pro",
		Body:           []byte(`{"model":"gemini-web/gemini-3.0-pro","messages":[{"role":"user","content":"hi"}]}`),
	})
	routeRes, err := p.HandleMethod(pluginabi.MethodModelRoute, routeReq)
	if err != nil {
		t.Fatalf("model.route failed: %v", err)
	}
	rResp := routeRes.(pluginapi.ModelRouteResponse)
	if !rResp.Handled || rResp.TargetKind != pluginapi.ModelRouteTargetSelf || rResp.TargetModel != "gemini-3.0-pro" {
		t.Fatalf("unexpected model.route response: %+v", rResp)
	}

	// 4. scheduler.pick
	schedReq, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "gemini-web2api",
		Model:    "gemini-3.0-pro",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{
				ID:         "gemini-web2api-user@example.com.json",
				Provider:   "gemini-web2api",
				Status:     "active",
				Attributes: map[string]string{"provider": "gemini-web2api", "account_id": "user@example.com"},
			},
		},
	})
	schedRes, err := p.HandleMethod(pluginabi.MethodSchedulerPick, schedReq)
	if err != nil {
		t.Fatalf("scheduler.pick failed: %v", err)
	}
	sResp := schedRes.(pluginapi.SchedulerPickResponse)
	if !sResp.Handled {
		t.Fatalf("expected scheduler.pick Handled=true, got: %+v", sResp)
	}

	// 5. quota.describe & quota.fetch
	qDesc, err := p.HandleMethod(pluginabi.MethodQuotaDescribe, nil)
	if err != nil {
		t.Fatalf("quota.describe failed: %v", err)
	}
	if !qDesc.(pluginapi.QuotaDescribeResponse).SupportsReset {
		t.Fatalf("expected SupportsReset=true")
	}

	qFetch, err := p.HandleMethod(pluginabi.MethodQuotaFetch, []byte(`{"provider":"gemini-web2api"}`))
	if err != nil {
		t.Fatalf("quota.fetch failed: %v", err)
	}
	if qFetch.(pluginapi.QuotaFetchResponse).Subscription == nil {
		t.Fatalf("expected Subscription in quota.fetch")
	}

	// 6. management.register & management.handle (Web UI & Status API)
	mgmtReg, err := p.HandleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatalf("management.register failed: %v", err)
	}
	mgmtRegJSON, _ := json.Marshal(mgmtReg)
	if !strings.Contains(string(mgmtRegJSON), `"/accounts"`) {
		t.Fatalf("missing resource route in management.register: %s", string(mgmtRegJSON))
	}

	uiReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/gemini-web2api/accounts",
	})
	uiRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, uiReq)
	if err != nil {
		t.Fatalf("management.handle UI failed: %v", err)
	}
	uiResp := uiRes.(pluginapi.ManagementResponse)
	if uiResp.StatusCode != http.StatusOK || !strings.Contains(string(uiResp.Body), "启动本机 Chrome 无痕登录") {
		t.Fatalf("unexpected UI response: status=%d len=%d", uiResp.StatusCode, len(uiResp.Body))
	}

	resStatusReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/gemini-web2api/accounts",
		Query:  map[string][]string{"view": {"status"}},
	})
	resStatusRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, resStatusReq)
	if err != nil {
		t.Fatalf("management.handle resource with query failed: %v", err)
	}
	resStatusResp := resStatusRes.(pluginapi.ManagementResponse)
	if resStatusResp.StatusCode != http.StatusOK || !strings.Contains(resStatusResp.Headers.Get("Content-Type"), "text/html") || strings.Contains(string(resStatusResp.Body), `"routing_strategy":"round-robin"`) {
		t.Fatalf("expected static HTML only on /v0/resource/plugins/..., got content-type=%q", resStatusResp.Headers.Get("Content-Type"))
	}

	statusReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/gemini-web2api/status",
	})
	statusRes, err := p.HandleMethod(pluginabi.MethodManagementHandle, statusReq)
	if err != nil {
		t.Fatalf("management.handle status failed: %v", err)
	}
	statusResp := statusRes.(pluginapi.ManagementResponse)
	if statusResp.StatusCode != http.StatusOK || !strings.Contains(string(statusResp.Body), `"routing_strategy":"round-robin"`) || !strings.Contains(string(statusResp.Body), `"chrome_available"`) {
		t.Fatalf("unexpected status response: %s", string(statusResp.Body))
	}

	t.Setenv("GEMINI_WEB2API_NO_AUTO_BROWSER", "1")
	startRes, err := p.HandleMethod(pluginabi.MethodAuthLoginStart, []byte(`{"provider":"gemini-web2api","base_url":"http://127.0.0.1:8317/v8/management/oauth/callback"}`))
	if err != nil {
		t.Fatalf("auth.login_start failed: %v", err)
	}
	startResp := startRes.(pluginapi.AuthLoginStartResponse)
	if startResp.URL != "http://127.0.0.1:8317/v0/resource/plugins/gemini-web2api/accounts" {
		t.Fatalf("expected valid full URL in AuthLoginStartResponse, got %q", startResp.URL)
	}

	// 7. executor.identifier & executor.count_tokens
	execIDRes, err := p.HandleMethod(pluginabi.MethodExecutorIdentifier, nil)
	if err != nil {
		t.Fatalf("executor.identifier failed: %v", err)
	}
	execIDJSON, _ := json.Marshal(execIDRes)
	if !strings.Contains(string(execIDJSON), `"gemini-web2api"`) {
		t.Fatalf("unexpected executor.identifier: %s", string(execIDJSON))
	}

	countReq, _ := json.Marshal(pluginapi.ExecutorRequest{
		Model:   "gemini-2.5-flash",
		Format:  "chat-completions",
		Payload: []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"Hello world from Gemini Web2API plugin!"}]}`),
	})
	countRes, err := p.HandleMethod(pluginabi.MethodExecutorCountTokens, countReq)
	if err != nil {
		t.Fatalf("executor.count_tokens failed: %v", err)
	}
	countResp := countRes.(pluginapi.ExecutorResponse)
	if !strings.Contains(string(countResp.Payload), `"totalTokens"`) {
		t.Fatalf("expected totalTokens from SentencePiece tokenizer, got %s", string(countResp.Payload))
	}
}

func TestAuthFileDisabledSyncOnServiceStopAndShutdown(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	authDir := filepath.Join(tmpHome, ".cli-proxy-api")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("mkdir authDir: %v", err)
	}
	dataDir := filepath.Join(tmpHome, ".gemini-web2api")
	authFilePath := filepath.Join(authDir, "gemini-web2api-testuser@gmail.com.json")
	initialPayload := `{
		"type": "gemini-web2api",
		"provider": "gemini-web2api",
		"account_id": "testuser@gmail.com",
		"email": "testuser@gmail.com",
		"label": "testuser@gmail.com",
		"disabled": true,
		"storage_state": {
			"cookies": [
				{"name": "__Secure-1PSID", "value": "test-1psid-val", "domain": ".google.com", "path": "/", "secure": true, "sameSite": "None"},
				{"name": "__Secure-1PSIDTS", "value": "test-1psidts-val", "domain": ".google.com", "path": "/", "secure": true, "sameSite": "None"},
				{"name": "__Secure-3PSID", "value": "test-3psid-val", "domain": ".google.com", "path": "/", "secure": true, "sameSite": "None"},
				{"name": "SID", "value": "test-sid-val", "domain": ".google.com", "path": "/", "sameSite": "Lax"}
			],
			"origins": []
		}
	}`
	if err := os.WriteFile(authFilePath, []byte(initialPayload), 0o600); err != nil {
		t.Fatalf("write initial auth file: %v", err)
	}

	p := cpa.NewPlugin(nil)
	defer func() {
		_, _ = p.HandleMethod(pluginabi.MethodPluginShutdown, []byte(`{}`))
	}()

	cfgYAML := []byte("data_dir: \"" + dataDir + "\"\nauto_start: false\nguest_enabled: false\ninit_timeout: 500ms\nsync_auth_files: true\n")
	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    cfgYAML,
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginRegister, regReq); err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}
	// Simulate auto_start=true runtime state for startup sync test
	cfgYAMLAuto := []byte("data_dir: \"" + dataDir + "\"\nauto_start: true\nguest_enabled: false\ninit_timeout: 500ms\nsync_auth_files: true\n")
	regReqAuto, _ := json.Marshal(map[string]any{
		"config_yaml":    cfgYAMLAuto,
		"schema_version": pluginabi.SchemaVersion,
	})
	if _, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, regReqAuto); err != nil {
		t.Fatalf("plugin.reconfigure failed: %v", err)
	}

	// 1. Parse auth during startup window -> should import account as enabled and flip disabled=false on disk
	parseReq, _ := json.Marshal(pluginapi.AuthParseRequest{
		FileName: "gemini-web2api-testuser@gmail.com.json",
		RawJSON:  []byte(initialPayload),
	})
	parseRes, err := p.HandleMethod(pluginabi.MethodAuthParse, parseReq)
	if err != nil {
		t.Fatalf("auth.parse failed: %v", err)
	}
	parseResp := parseRes.(pluginapi.AuthParseResponse)
	if !parseResp.Handled || parseResp.Auth.Disabled {
		t.Fatalf("expected auth to be enabled on plugin startup, got Disabled=%v", parseResp.Auth.Disabled)
	}

	checkDiskDisabled := func(wantDisabled bool, wantStopped bool, step string) {
		t.Helper()
		raw, err := os.ReadFile(authFilePath)
		if err != nil {
			t.Fatalf("%s: read auth file: %v", step, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: unmarshal auth file: %v", step, err)
		}
		gotDisabled, _ := doc["disabled"].(bool)
		gotStopped, _ := doc["service_stopped"].(bool)
		if gotDisabled != wantDisabled || gotStopped != wantStopped {
			t.Fatalf("%s: disk auth file disabled=%v service_stopped=%v, want disabled=%v service_stopped=%v", step, gotDisabled, gotStopped, wantDisabled, wantStopped)
		}
	}
	checkDiskDisabled(false, false, "after startup auth.parse")

	// 2. Stop service via management API -> should mark disk auth file disabled=true, service_stopped=true
	stopReq, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/gemini-web2api/service/stop",
	})
	if _, err := p.HandleMethod(pluginabi.MethodManagementHandle, stopReq); err != nil {
		t.Fatalf("service/stop failed: %v", err)
	}
	checkDiskDisabled(true, true, "after service/stop")

	// 3. Re-enable plugin (reconfigure with auto_start=true) -> should restore disabled=false, service_stopped=false
	if _, err := p.HandleMethod(pluginabi.MethodPluginReconfigure, regReqAuto); err != nil {
		t.Fatalf("plugin.reconfigure failed: %v", err)
	}
	checkDiskDisabled(false, false, "after plugin reconfigure/enable")

	// 4. Quiesce/Shutdown plugin -> should mark disk auth file disabled=true, service_stopped=true
	if _, err := p.HandleMethod(pluginabi.MethodPluginQuiesce, []byte(`{}`)); err != nil {
		t.Fatalf("plugin.quiesce failed: %v", err)
	}
	checkDiskDisabled(true, true, "after plugin.quiesce")
}

func TestBuiltDynamicLibraryExists(t *testing.T) {
	dylibPath := filepath.Join("..", "..", "dist", "plugins", "darwin", "arm64", "gemini-web2api.dylib")
	info, err := os.Stat(dylibPath)
	if err != nil {
		t.Skipf("dylib not found at %s: %v", dylibPath, err)
	}
	if info.Size() < 1*1024*1024 {
		t.Fatalf("expected built dylib > 1MB, got %d bytes", info.Size())
	}
}
