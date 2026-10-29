package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
	"knot-cli/internal/i18n"
)

type managementRequest struct {
	Method, Path string
	Body         map[string]json.RawMessage
}
type managementFixture struct {
	mu          sync.Mutex
	Requests    []managementRequest
	KeyAttempts int
}

func managementModel(t *testing.T, page string) (*Model, *managementFixture) {
	t.Helper()
	f := &managementFixture{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]json.RawMessage{}
		if r.Body != nil && r.ContentLength != 0 {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.Requests = append(f.Requests, managementRequest{r.Method, r.URL.Path, body})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/runtime":
			_, _ = w.Write([]byte(`{"data":{"instance_id":"instance-a"}}`))
		case r.URL.Path == "/v1/proxy-tests":
			_, _ = w.Write([]byte(`{"data":{"passed":true,"scope":"endpoint","stages":[{"name":"endpoint","passed":true,"duration_ms":1}]}}`))
		case strings.HasSuffix(r.URL.Path, "/private") && r.Method == "PUT":
			f.KeyAttempts++
			if f.KeyAttempts == 1 {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"code":"PASSPHRASE_REQUIRED"}}`))
			} else {
				_, _ = w.Write([]byte(`{"data":{}}`))
			}
		case r.Method == "GET" && r.URL.Path == "/v1/config/servers/opaque-a":
			_, _ = w.Write([]byte(`{"data":{"id":"opaque-a","alias":"alpha","host":"example","port":22,"user":"ops","auth_method":"agent","known_hosts_path":"/keep/known_hosts"}}`))
		case strings.HasSuffix(r.URL.Path, "/forwards") && r.Method == "GET":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.URL.Path == "/v1/forwards" && r.Method == "POST":
			_, _ = w.Write([]byte(`{"data":{"id":"forward_actual","server_id":"opaque-a","connection_id":"conn_actual","type":"dynamic","bind":"127.0.0.1:0","state":"starting","origin":"temporary"}}`))
		default:
			if len(body) == 0 {
				body["deleted"] = json.RawMessage(`true`)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": body})
		}
	}))
	t.Cleanup(s.Close)
	m := testModel(t, page)
	c, err := coreclient.New(s.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	m.snapshot.Client.Close()
	m.snapshot.Client = c
	return m, f
}
func (f *managementFixture) records() []managementRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]managementRequest(nil), f.Requests...)
}
func control(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Mod: tea.ModCtrl})
}
func focusField(t *testing.T, f *Form, name string) {
	t.Helper()
	for i, field := range f.Fields {
		if field.Name == name {
			f.Focus(i)
			return
		}
	}
	t.Fatal("field missing", name)
}
func saveMessage(t *testing.T, m *Model) manageResult {
	t.Helper()
	cmd := m.submitForm()
	if cmd == nil {
		t.Fatal("save command missing")
	}
	msg := cmd()
	result, ok := msg.(manageResult)
	if !ok {
		t.Fatalf("save returned %T", msg)
	}
	return result
}

func TestServerEditorSavesTypedMetadataDefinitionsAndPassword(t *testing.T) {
	m, f := managementModel(t, "servers")
	m.openEditor(coreclient.Servers, true, false)
	modal := m.modal
	for name, value := range map[string]string{"alias": "new", "host": "example", "user": "ops", "auth_method": "password", "password": "secret", "tags": "Production, 中文"} {
		setField(modal.Form, name, value)
	}
	if len(modal.Form.Sections) != 4 {
		t.Fatal("server sections missing")
	}
	modal.Form.switchSection(1)
	focusField(t, modal.Form, "proxy_id")
	m.Update(press("right"))
	if modal.Form.Values()["proxy_id"] != "proxy" || !strings.Contains(ansi.Strip(m.View().Content), "corp") {
		t.Fatal("proxy selector must display alias")
	}
	modal.Form.switchSection(1)
	m.Update(control('n'))
	setField(modal.Form, "jump.0", "opaque-b")
	for _, field := range modal.Form.Fields {
		if strings.HasPrefix(field.Name, "jump.") {
			for _, c := range field.Choices {
				if c.ID == modal.Edit.ID {
					t.Fatal("self offered as jump")
				}
			}
		}
	}
	modal.Form.switchSection(1)
	m.Update(control('n'))
	setField(modal.Form, "forward.0.bind", "127.0.0.1:8080")
	setField(modal.Form, "forward.0.target", "localhost:80")
	result := saveMessage(t, m)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	records := f.records()
	if len(records) != 4 || records[1].Path != "/v1/config/servers" || !strings.HasSuffix(records[2].Path, "/forwards") || !strings.HasSuffix(records[3].Path, "/password") {
		t.Fatalf("wrong write order: %+v", records)
	}
	for _, key := range []string{"password", "password_set", "last_used", "forwards"} {
		if _, exists := records[1].Body[key]; exists {
			t.Fatal("ordinary DTO leaked", key)
		}
	}
	if string(records[1].Body["jump_host_ids"]) != `["opaque-b"]` {
		t.Fatal("jump IDs not preserved")
	}
	m.Update(result)
	if m.modal != nil || m.selectedID != result.ID {
		t.Fatal("successful save did not close and select resource")
	}
}

func TestEditorReadsAndPreservesHiddenServerFields(t *testing.T) {
	m, f := managementModel(t, "servers")
	cmd := m.openEditor(coreclient.Servers, false, false)
	// OpenModal has no focus command for the loading information view.
	var load editorResult
	var visit func(tea.Cmd)
	visit = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			for _, child := range msg {
				visit(child)
			}
		case editorResult:
			load = msg
		}
	}
	visit(cmd)
	if load.Err != nil {
		t.Fatal(load.Err)
	}
	m.Update(load)
	setField(m.modal.Form, "alias", "renamed")
	result := saveMessage(t, m)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	for _, r := range f.records() {
		if r.Method == "PUT" && r.Path == "/v1/config/servers/opaque-a" && string(r.Body["known_hosts_path"]) != `"/keep/known_hosts"` {
			t.Fatal("hidden server field lost")
		}
	}
}

func TestPrivateKeyPromptKeepsMaterialAndLocatesPassphrase(t *testing.T) {
	m, f := managementModel(t, "keys")
	m.openEditor(coreclient.Keys, true, false)
	modal := m.modal
	setField(modal.Form, "alias", "identity")
	setField(modal.Form, "source", "inline")
	setField(modal.Form, "private_key", "private\nmaterial\n")
	first := saveMessage(t, m)
	if coreclient.Code(first.Err) != "PASSPHRASE_REQUIRED" {
		t.Fatal(first.Err)
	}
	m.Update(first)
	if modal.Form.Fields[modal.Form.Index].Name != "passphrase" || modal.Form.Values()["private_key"] != "private\nmaterial\n" || modal.Edit.Create {
		t.Fatal("key prompt lost draft or created identity")
	}
	setField(modal.Form, "passphrase", "passphrase")
	second := saveMessage(t, m)
	if second.Err != nil {
		t.Fatal(second.Err)
	}
	m.Update(second)
	posts := 0
	for _, r := range f.records() {
		if r.Method == "POST" && r.Path == "/v1/config/keys" {
			posts++
		}
	}
	if posts != 1 || m.modal != nil {
		t.Fatal("key import did not finish on original key")
	}
}

func TestConditionalFieldsTabsAndJumpReordering(t *testing.T) {
	m := testModel(t, "servers")
	m.openEditor(coreclient.Servers, true, false)
	modal := m.modal
	f := modal.Form
	setField(f, "alias", "new")
	setField(f, "host", "host")
	setField(f, "user", "ops")
	f.Validate("required")
	if f.Invalid() {
		t.Fatal("hidden key field required for agent")
	}
	setField(f, "auth_method", "key")
	cmd := m.saveResource(modal, f.Values())
	if cmd != nil {
		cmd()
	}
	if f.Fields[f.Index].Name != "key_id" {
		t.Fatal("missing key not located")
	}
	f.switchSection(1)
	f.switchSection(1)
	m.Update(control('n'))
	setField(f, "jump.0", "opaque-a")
	m.Update(control('n'))
	setField(f, "jump.1", "opaque-b")
	m.Update(control(tea.KeyUp))
	if f.Values()["jump.0"] != "opaque-b" {
		t.Fatal("jump order not changed")
	}
	m.Update(control('d'))
	if _, ok := f.Values()["jump.1"]; ok || f.Values()["jump.0"] != "opaque-a" {
		t.Fatal("jump removal not renumbered")
	}
	m.snapshot.Config.Servers[0].JumpHostIDs = []string{modal.Edit.ID}
	if !m.jumpCycle(modal.Edit.ID, []string{m.snapshot.Config.Servers[0].ID}) {
		t.Fatal("cycle not detected")
	}
}

func TestSyncPasswordGateAndS3EndpointDefaults(t *testing.T) {
	m, _ := managementModel(t, "sync")
	m.Update(press("s"))
	if m.modal == nil {
		t.Fatal("empty sync gate did not open password form")
	}
	setField(m.modal.Form, "password", "new")
	setField(m.modal.Form, "confirm_password", "different")
	cmd := m.submitForm()
	if cmd != nil {
		cmd()
	}
	if m.modal.Form.Busy || m.modal.Form.Fields[m.modal.Form.Index].Name != "confirm_password" {
		t.Fatal("password mismatch not located")
	}
	setField(m.modal.Form, "confirm_password", "new")
	result := saveMessage(t, m)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	m.Update(result)
	m.openEditor(coreclient.Providers, true, false)
	f := m.modal.Form
	setField(f, "type", "s3")
	focusField(t, f, "endpoint")
	m.Update(tea.PasteMsg{Content: "https://s3.example.invalid"})
	if f.Values()["region"] != "auto" || f.Values()["key"] != "config.toml.enc" {
		t.Fatal("S3 defaults missing")
	}
	setField(f, "region", "existing")
	m.Update(tea.PasteMsg{Content: "/path"})
	if f.Values()["region"] != "existing" {
		t.Fatal("existing S3 region overwritten")
	}
}

func TestDraftProxyTestDoesNotSaveAndReportsScope(t *testing.T) {
	m, f := managementModel(t, "proxy")
	m.openEditor(coreclient.Proxies, true, false)
	modal := m.modal
	setField(modal.Form, "host", "127.0.0.1")
	setField(modal.Form, "type", "http")
	cmd := m.testDraftProxy(modal)
	var result proxyResult
	var visit func(tea.Cmd)
	visit = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			for _, child := range msg {
				visit(child)
			}
		case proxyResult:
			result = msg
		}
	}
	visit(cmd)
	m.Update(result)
	if m.modal != modal || !strings.Contains(modal.Body, "TCP endpoint reachability only") {
		t.Fatal("proxy scope missing or draft replaced")
	}
	for _, r := range f.records() {
		if strings.HasPrefix(r.Path, "/v1/config") {
			t.Fatal("proxy test saved draft")
		}
	}
	m.modal = nil
	m.openEditor(coreclient.Proxies, true, false)
	current := m.modal
	m.Update(result)
	if current.Body != "" || current.Hints != m.tr.Message("manage.proxy_form_hint") {
		t.Fatal("late test changed new draft")
	}
}

func TestForwardManagerUsesActualConnectionAndSeparatesDefinitions(t *testing.T) {
	m, f := managementModel(t, "servers")
	manager := &forwardManager{ServerID: "opaque-a"}
	m.openTemporaryForward(manager)
	modal := m.modal
	setField(modal.Form, "type", "dynamic")
	cmd := m.submitForm()
	if cmd == nil {
		t.Fatal("missing temporary creation")
	}
	msg, ok := cmd().(forwardActionResult)
	if !ok || msg.Err != nil || msg.Forward.ConnectionID != "conn_actual" {
		t.Fatal("actual connection lost", msg.Err)
	}
	m.Update(msg)
	if m.modal.Forwards == nil || m.modal.Forwards.Saved {
		t.Fatal("did not return to runtime manager")
	}
	for _, r := range f.records() {
		if r.Path == "/v1/forwards" {
			if _, ok := r.Body["connection_id"]; ok {
				t.Fatal("latest connection chosen by frontend")
			}
		}
		if strings.HasPrefix(r.Path, "/v1/config") && r.Method != "GET" {
			t.Fatal("temporary rule persisted")
		}
	}
	modal = m.modal
	m.handleForwards(forwardsResult{ModalID: modal.ID, Instance: "instance-a", Generation: modal.Forwards.Generation, Runtime: []coreclient.Forward{{ID: "forward_actual", ServerID: "opaque-a", ConnectionID: "conn_actual", Origin: "temporary", State: "failed", FailureCode: "FORWARD_START_FAILED", Type: "dynamic", Bind: "127.0.0.1:1080"}}})
	if !strings.Contains(m.forwardDetail(modal), "could not start") {
		t.Fatal("real forwarding failure hidden")
	}
	m.Update(press("]"))
	if !modal.Forwards.Saved {
		t.Fatal("saved/running tab did not switch")
	}
	page := m.page
	m.Update(press("right"))
	if m.page != page {
		t.Fatal("manager key escaped")
	}
}

func TestResourceFormsFitBilingualSizesAndKeepInputKeys(t *testing.T) {
	for _, lang := range []i18n.Language{i18n.English, i18n.Chinese} {
		for _, size := range [][2]int{{80, 24}, {120, 35}} {
			for _, kind := range []coreclient.Resource{coreclient.Servers, coreclient.Keys, coreclient.Proxies, coreclient.Providers} {
				t.Run(fmt.Sprintf("%s/%dx%d/%s", lang, size[0], size[1], kind), func(t *testing.T) {
					m := testModel(t, "servers")
					m.tr = i18n.New(lang)
					m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
					m.openEditor(kind, true, false)
					modal := m.modal
					m.Update(press("n"))
					m.Update(press("t"))
					if m.modal != modal || !strings.Contains(modal.Form.Values()["alias"], "nt") {
						t.Fatal("typing triggered business action")
					}
					for i := range modal.Form.Fields {
						if !modal.Form.visible(i) {
							continue
						}
						modal.Form.Focus(i)
						view := ansi.Strip(m.View().Content)
						checkSize(t, view, size)
						if !strings.Contains(view, "Ctrl+s") {
							t.Fatal("save hint hidden")
						}
					}
				})
			}
		}
	}
}

func TestResourceConfirmationIsExclusiveAndInstanceBound(t *testing.T) {
	m, _ := managementModel(t, string(entry.Keys))
	m.confirmResourceAction(coreclient.Keys, "material")
	for _, k := range []string{"n", "?", "ctrl+c"} {
		m.Update(press(k))
		if m.modal == nil || m.modal.Busy {
			t.Fatal("confirmation leaked keys")
		}
	}
	m.snapshot.Runtime.InstanceID = "different"
	m.Update(press("enter"))
	if m.modal.Busy {
		t.Fatal("stale confirmation submitted")
	}
	m.Update(press("esc"))
	if m.modal != nil {
		t.Fatal("confirmation not canceled")
	}
}

func TestSavedDefinitionDeletionDoesNotStopRuntime(t *testing.T) {
	m, f := managementModel(t, "servers")
	definition := coreclient.ForwardDefinition{ID: "saved", Type: "dynamic", Bind: "127.0.0.1:1080", Enabled: true}
	manager := &forwardManager{ServerID: "opaque-a", Saved: true, Definitions: []coreclient.ForwardDefinition{definition}}
	m.confirmDefinitionDelete(manager, definition)
	_, cmd := m.Update(press("enter"))
	if cmd == nil {
		t.Fatal("definition deletion not submitted")
	}
	result, ok := cmd().(forwardActionResult)
	if !ok || result.Err != nil {
		t.Fatal("definition deletion failed", result.Err)
	}
	requests := f.records()
	if len(requests) != 2 || requests[1].Method != "PUT" || requests[1].Path != "/v1/config/servers/opaque-a/forwards" {
		t.Fatal("definition delete changed runtime", requests)
	}
}
