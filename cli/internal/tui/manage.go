package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"knot-cli/internal/coreclient"
	"knot-cli/internal/entry"
)

type resourceDraft struct {
	Kind            coreclient.Resource
	ID              string
	Create          bool
	Server          coreclient.ServerWrite
	Proxy           coreclient.ProxyWrite
	Provider        coreclient.ProviderWrite
	Alias           string
	Definitions     []coreclient.ForwardDefinition
	CredentialsOnly bool
	Section         string
}

type editorResult struct {
	ModalID  uint64
	Instance string
	Draft    resourceDraft
	Err      error
}
type manageResult struct {
	ModalID  uint64
	Instance string
	ID       string
	Created  bool
	Err      error
	Fields   map[string]string
	Message  string
}

func newResourceID(kind coreclient.Resource) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", coreclient.Failure("INTERNAL_ERROR")
	}
	prefix := map[coreclient.Resource]string{coreclient.Servers: "srv", coreclient.Keys: "key", coreclient.Proxies: "prx", coreclient.Providers: "sync"}[kind]
	return prefix + "_" + hex.EncodeToString(b[:]), nil
}

func (m *Model) businessAction(key string) tea.Cmd {
	if m.page == entry.Servers && key == "c" {
		return m.startSSH(m.selectedID, false)
	}
	kind := map[entry.Page]coreclient.Resource{entry.Servers: coreclient.Servers, entry.Keys: coreclient.Keys, entry.Proxies: coreclient.Proxies, entry.Sync: coreclient.Providers}[m.page]
	if m.page == entry.Sync && key == "s" {
		return m.openSyncPassword()
	}
	if m.page == entry.Proxies && key == "t" {
		return m.testSavedProxy()
	}
	if key == "n" || key == "e" {
		return m.openEditor(kind, key == "n", false)
	}
	if key == "p" && (kind == coreclient.Servers || kind == coreclient.Providers) {
		return m.openEditor(kind, false, true)
	}
	if key == "d" || key == "delete" {
		return m.confirmResourceAction(kind, "delete")
	}
	if kind == coreclient.Keys && key == "x" {
		return m.confirmResourceAction(kind, "material")
	}
	if kind == coreclient.Providers && key == "a" {
		return m.confirmResourceAction(kind, "default")
	}
	return m.pending()
}

func (m *Model) openEditor(kind coreclient.Resource, create, credentials bool, sections ...string) tea.Cmd {
	d := resourceDraft{Kind: kind, ID: m.selectedID, Create: create, CredentialsOnly: credentials}
	if len(sections) > 0 {
		d.Section = sections[0]
	}
	if create {
		var err error
		d.ID, err = newResourceID(kind)
		if err != nil {
			return m.notify(NoticeMsg{ID: "edit", Text: m.tr.ProtocolError(coreclient.Code(err)), Error: true})
		}
		d.Server = coreclient.ServerWrite{ID: d.ID, Port: 22, AuthMethod: "agent"}
		d.Proxy = coreclient.ProxyWrite{ID: d.ID, Type: "socks5", Port: 1080}
		d.Provider = coreclient.ProviderWrite{ID: d.ID, Type: "webdav", Key: "config.toml.enc"}
		return m.showEditor(d)
	}
	modal := &Modal{Kind: Information, Title: m.editorTitle(kind, false), Body: m.tr.Message("ui.loading")}
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	modal.OnCancel = func() tea.Msg { cancel(); return nil }
	cmd := m.OpenModal(modal)
	client, instance, id := m.snapshot.Client, m.snapshot.Runtime.InstanceID, modal.ID
	return tea.Batch(cmd, func() tea.Msg {
		defer cancel()
		err := client.CheckInstance(ctx, instance)
		if err == nil {
			switch kind {
			case coreclient.Servers:
				var s coreclient.Server
				s, err = client.Server(ctx, d.ID)
				d.Server = coreclient.ServerInput(s)
				if err == nil && !credentials {
					d.Definitions, err = client.ForwardDefinitions(ctx, d.ID)
				}
			case coreclient.Proxies:
				var p coreclient.Proxy
				p, err = client.Proxy(ctx, d.ID)
				d.Proxy = coreclient.ProxyInput(p)
			case coreclient.Providers:
				var p coreclient.SyncProvider
				p, err = client.Provider(ctx, d.ID)
				d.Provider = coreclient.ProviderInput(p)
			}
		}
		return editorResult{ModalID: id, Instance: instance, Draft: d, Err: err}
	})
}

func (m *Model) text(name, key, value string, required, secret bool) Field {
	return TextField(name, m.tr.Message(key), value, required, secret)
}
func (m *Model) choice(name, key, value string, choices ...Choice) Field {
	return ChoiceField(name, m.tr.Message(key), value, choices...)
}
func (m *Model) boolField(name, key string, value bool) Field {
	return m.choice(name, key, strconv.FormatBool(value), Choice{ID: "false", Label: m.tr.Message("ui.no")}, Choice{ID: "true", Label: m.tr.Message("ui.yes")})
}
func when(name string, values ...string) func(map[string]string) bool {
	return func(v map[string]string) bool {
		for _, value := range values {
			if v[name] == value {
				return true
			}
		}
		return false
	}
}
func conditional(f Field, name string, values ...string) Field {
	f.When = when(name, values...)
	return f
}
func section(name string, fields ...Field) []Field {
	for i := range fields {
		fields[i].Section = name
	}
	return fields
}

func (m *Model) passwordFields() []Field {
	return []Field{m.text("password", "manage.password_keep", "", false, true), m.boolField("clear_password", "manage.clear_password", false)}
}

func (m *Model) showEditor(d resourceDraft) tea.Cmd {
	var fields []Field
	if !d.CredentialsOnly {
		alias := d.Alias
		switch d.Kind {
		case coreclient.Servers:
			alias = d.Server.Alias
		case coreclient.Proxies:
			alias = d.Proxy.Alias
		case coreclient.Providers:
			alias = d.Provider.Alias
		}
		fields = append(fields, m.text("alias", "ui.alias", alias, true, false))
	}
	switch d.Kind {
	case coreclient.Servers:
		if d.CredentialsOnly {
			fields = m.passwordFields()
			break
		}
		s := d.Server
		fields = append(fields, m.text("host", "manage.host", s.Host, true, false), m.text("port", "manage.port", strconv.Itoa(s.Port), true, false), m.text("user", "manage.user", s.User, true, false), m.choice("auth_method", "ui.auth_method", s.AuthMethod, Choice{ID: "agent", Label: m.auth("agent")}, Choice{ID: "key", Label: m.auth("key")}, Choice{ID: "password", Label: m.auth("password")}))
		keys := []Choice{{ID: "", Label: m.tr.Message("ui.none")}}
		for _, key := range m.snapshot.Config.Keys {
			keys = append(keys, Choice{ID: key.ID, Label: key.Alias})
		}
		fields = append(fields, conditional(m.choice("key_id", "ui.auth.key", s.KeyID, keys...), "auth_method", "key"))
		for _, f := range m.passwordFields() {
			fields = append(fields, conditional(f, "auth_method", "password"))
		}
		fields = append(fields, m.text("tags", "manage.tags", strings.Join(s.Tags, ", "), false, false))
		fields = section("basic", fields...)
		proxies := []Choice{{ID: "", Label: m.tr.Message("ui.none")}}
		for _, p := range m.snapshot.Config.Proxies {
			proxies = append(proxies, Choice{ID: p.ID, Label: p.Alias})
		}
		fields = append(fields, section("proxy", m.choice("proxy_id", "ui.proxy", s.ProxyID, proxies...))...)
		jumpHelp := m.text("jump_help", "manage.jump_help", "", false, false)
		jumpHelp.Static = true
		fields = append(fields, section("jumps", jumpHelp)...)
		for i, id := range s.JumpHostIDs {
			fields = append(fields, m.jumpField(d.ID, i, id))
		}
		forwardHelp := m.text("forward_help", "manage.forward_help", "", false, false)
		forwardHelp.Static = true
		fields = append(fields, section("forwards", forwardHelp)...)
		for i, f := range d.Definitions {
			fields = append(fields, m.definitionFields(i, f)...)
		}
	case coreclient.Keys:
		fields = append(fields, m.choice("source", "manage.source", "file", Choice{ID: "file", Label: m.tr.Message("manage.file")}, Choice{ID: "inline", Label: m.tr.Message("manage.inline")}))
		fields = append(fields, conditional(m.text("source_path", "manage.source_path", "", true, false), "source", "file"), conditional(MultilineField("private_key", m.tr.Message("ui.material"), "", true), "source", "inline"), m.text("passphrase", "manage.passphrase", "", false, true))
	case coreclient.Proxies:
		p := d.Proxy
		fields = append(fields, m.choice("type", "ui.type", p.Type, Choice{ID: "socks5", Label: "SOCKS5"}, Choice{ID: "http", Label: "HTTP"}), m.text("host", "manage.host", p.Host, true, false), m.text("port", "manage.port", strconv.Itoa(p.Port), true, false), m.text("username", "ui.username", p.Username, false, false))
		fields = append(fields, m.passwordFields()...)
		fields = append(fields, m.text("test_target", "manage.test_target", "", false, false))
	case coreclient.Providers:
		p := d.Provider
		if !d.CredentialsOnly {
			fields = append(fields, m.choice("type", "ui.type", p.Type, Choice{ID: "webdav", Label: "WebDAV"}, Choice{ID: "s3", Label: "S3"}), conditional(m.text("url", "manage.url", p.URL, true, false), "type", "webdav"), conditional(m.text("username", "ui.username", p.Username, false, false), "type", "webdav"))
			for _, f := range []Field{m.text("bucket", "ui.bucket", p.Bucket, true, false), m.text("key", "ui.object_key", p.Key, true, false), m.text("region", "ui.region", p.Region, true, false), m.text("endpoint", "ui.endpoint", p.Endpoint, false, false), m.boolField("path_style", "ui.path_style", p.PathStyle)} {
				fields = append(fields, conditional(f, "type", "s3"))
			}
		}
		for _, f := range m.passwordFields() {
			if d.CredentialsOnly {
				if p.Type == "webdav" {
					fields = append(fields, f)
				}
			} else {
				fields = append(fields, conditional(f, "type", "webdav"))
			}
		}
		for _, f := range []Field{m.text("access_key_id", "manage.access_key_id", "", false, true), m.text("secret_access_key", "manage.secret_access_key", "", false, true), m.text("session_token", "manage.session_token", "", false, true), m.boolField("clear_s3", "manage.clear_s3", false)} {
			if d.CredentialsOnly {
				if p.Type == "s3" {
					fields = append(fields, f)
				}
			} else {
				fields = append(fields, conditional(f, "type", "s3"))
			}
		}
	}
	form := NewForm(fields...)
	if d.Kind == coreclient.Servers && !d.CredentialsOnly {
		form.Sections = []Choice{{ID: "basic", Label: m.tr.Message("manage.basic")}, {ID: "proxy", Label: m.tr.Message("ui.proxy")}, {ID: "jumps", Label: m.tr.Message("ui.jumps")}, {ID: "forwards", Label: m.tr.Message("manage.forwards")}}
	}
	title := m.editorTitle(d.Kind, d.Create)
	modal := &Modal{Kind: EditForm, Title: title, Form: form, Edit: &d}
	if d.Kind == coreclient.Proxies {
		modal.Body = m.tr.Message("manage.proxy_form_hint")
	}
	modal.OnSubmit = func(v map[string]string) tea.Cmd { return m.saveResource(modal, v) }
	modal.OnKey = func(k tea.KeyPressMsg) (bool, tea.Cmd) {
		if d.Kind == coreclient.Proxies && k.String() == "ctrl+t" {
			return true, m.testDraftProxy(modal)
		}
		if d.Kind == coreclient.Servers && !d.CredentialsOnly {
			return m.serverEditorKey(modal, k)
		}
		return false, nil
	}
	lastEndpoint := d.Provider.Endpoint
	modal.OnChange = func() {
		v := form.Values()
		if d.Kind == coreclient.Providers && !d.CredentialsOnly && v["type"] == "s3" && v["endpoint"] != lastEndpoint {
			lastEndpoint = v["endpoint"]
			if v["endpoint"] != "" && v["region"] == "" {
				setField(form, "region", "auto")
			}
		}
	}
	cmd := m.OpenModal(modal)
	if d.Section != "" {
		for i, field := range form.Fields {
			if field.Section == d.Section {
				return tea.Batch(cmd, form.Focus(i))
			}
		}
	}
	return cmd
}

func (m *Model) editorTitle(kind coreclient.Resource, create bool) string {
	action := "edit"
	if create {
		action = "new"
	}
	return m.tr.Message("manage." + action + "." + string(kind))
}

func setField(form *Form, name, value string) {
	for i := range form.Fields {
		if form.Fields[i].Name == name {
			if form.Fields[i].Multiline {
				form.Fields[i].Area.SetValue(value)
			} else {
				form.Fields[i].Input.SetValue(value)
			}
			return
		}
	}
}

func (m *Model) jumpField(self string, index int, id string) Field {
	choices := []Choice{{ID: "", Label: m.tr.Message("ui.none")}}
	for _, s := range m.snapshot.Config.Servers {
		if s.ID != self {
			choices = append(choices, Choice{ID: s.ID, Label: s.Alias})
		}
	}
	f := m.choice(fmt.Sprintf("jump.%d", index), "ui.jumps", id, choices...)
	f.Section = "jumps"
	return f
}
func (m *Model) definitionFields(index int, d coreclient.ForwardDefinition) []Field {
	p := fmt.Sprintf("forward.%d.", index)
	return section("forwards", m.choice(p+"type", "ui.type", d.Type, m.forwardTypes()...), m.text(p+"bind", "manage.bind", d.Bind, true, false), m.text(p+"target", "manage.forward_target", d.Target, false, false), m.boolField(p+"enabled", "manage.enabled", d.Enabled))
}
func (m *Model) forwardTypes() []Choice {
	var out []Choice
	for _, kind := range []string{"local", "remote", "dynamic"} {
		out = append(out, Choice{ID: kind, Label: m.tr.Message("manage.forward." + kind)})
	}
	return out
}

func (m *Model) serverEditorKey(modal *Modal, key tea.KeyPressMsg) (bool, tea.Cmd) {
	f := modal.Form
	k := key.String()
	if k != "ctrl+n" && k != "ctrl+d" && k != "ctrl+up" && k != "ctrl+down" {
		return false, nil
	}
	if f.Section == "jumps" {
		var indices []int
		for i, field := range f.Fields {
			if strings.HasPrefix(field.Name, "jump.") {
				indices = append(indices, i)
			}
		}
		current := -1
		for n, i := range indices {
			if f.Index == i {
				current = n
			}
		}
		if k == "ctrl+n" {
			at := len(f.Fields)
			for i, field := range f.Fields {
				if field.Section == "forwards" {
					at = i
					break
				}
			}
			field := m.jumpField(modal.Edit.ID, len(indices), "")
			f.Fields = append(f.Fields[:at], append([]Field{field}, f.Fields[at:]...)...)
			return true, f.Focus(at)
		}
		if current >= 0 {
			i := indices[current]
			if k == "ctrl+d" {
				f.Fields = append(f.Fields[:i], f.Fields[i+1:]...)
				renumberFields(f, "jump.", 1)
				return true, f.Focus(max(0, i-1))
			}
			other := current - 1
			if k == "ctrl+down" {
				other = current + 1
			}
			if other >= 0 && other < len(indices) {
				j := indices[other]
				a, b := f.Fields[i].Value(), f.Fields[j].Value()
				f.Fields[i].Input.SetValue(b)
				f.Fields[j].Input.SetValue(a)
				return true, f.Focus(j)
			}
		}
	}
	if f.Section == "forwards" {
		count := 0
		for _, field := range f.Fields {
			if strings.HasPrefix(field.Name, "forward.") && strings.HasSuffix(field.Name, ".type") {
				count++
			}
		}
		if k == "ctrl+n" && count < 128 {
			at := len(f.Fields)
			f.Fields = append(f.Fields, m.definitionFields(count, coreclient.ForwardDefinition{Type: "local", Enabled: true})...)
			modal.Edit.Definitions = append(modal.Edit.Definitions, coreclient.ForwardDefinition{})
			return true, f.Focus(at)
		}
		if k == "ctrl+d" && f.Index >= 0 && strings.HasPrefix(f.Fields[f.Index].Name, "forward.") {
			n, _ := strconv.Atoi(strings.Split(f.Fields[f.Index].Name, ".")[1])
			prefix := fmt.Sprintf("forward.%d.", n)
			at := f.Index
			var kept []Field
			for i, field := range f.Fields {
				if !strings.HasPrefix(field.Name, prefix) {
					kept = append(kept, field)
				} else {
					at = min(at, i)
				}
			}
			f.Fields = kept
			if n < len(modal.Edit.Definitions) {
				modal.Edit.Definitions = append(modal.Edit.Definitions[:n], modal.Edit.Definitions[n+1:]...)
			}
			renumberFields(f, "forward.", 4)
			return true, f.Focus(max(0, at-1))
		}
	}
	return true, nil
}
func renumberFields(f *Form, prefix string, size int) {
	n := 0
	for i := range f.Fields {
		if strings.HasPrefix(f.Fields[i].Name, prefix) {
			suffix := ""
			if size > 1 {
				suffix = "." + strings.Split(f.Fields[i].Name, ".")[2]
			}
			f.Fields[i].Name = fmt.Sprintf("%s%d%s", prefix, n/size, suffix)
			n++
		}
	}
}

func (m *Model) formErrors(modal *Modal, fields map[string]string) tea.Cmd {
	modal.Form.Busy = false
	first := -1
	for i := range modal.Form.Fields {
		field := &modal.Form.Fields[i]
		field.Error = fields[field.Name]
		if first < 0 && field.Error != "" {
			first = i
		}
	}
	if first >= 0 {
		return modal.Form.Focus(first)
	}
	return nil
}
func listValues(value string) []string {
	var values []string
	for _, v := range strings.Split(value, ",") {
		if v = strings.TrimSpace(v); v != "" {
			values = append(values, v)
		}
	}
	return values
}

func (m *Model) saveResource(modal *Modal, v map[string]string) tea.Cmd {
	d := *modal.Edit
	fields := map[string]string{}
	port, _ := strconv.Atoi(v["port"])
	if !d.CredentialsOnly && (d.Kind == coreclient.Servers || d.Kind == coreclient.Proxies) && (port < 1 || port > 65535) {
		fields["port"] = m.tr.Message("manage.invalid_port")
	}
	if v["clear_password"] == "true" && v["password"] != "" {
		fields["password"] = m.tr.Message("manage.clear_conflict")
	}
	var defs []coreclient.ForwardDefinition
	if d.Kind == coreclient.Servers && !d.CredentialsOnly {
		var jumps []string
		for i := 0; ; i++ {
			value, ok := v[fmt.Sprintf("jump.%d", i)]
			if !ok {
				break
			}
			if value != "" {
				jumps = append(jumps, value)
			}
		}
		if m.jumpCycle(d.ID, jumps) {
			fields["jump_help"] = m.tr.Message("manage.jump_cycle")
		}
		d.Server = coreclient.ServerWrite{ID: d.ID, Alias: strings.TrimSpace(v["alias"]), Host: strings.TrimSpace(v["host"]), Port: port, User: strings.TrimSpace(v["user"]), AuthMethod: v["auth_method"], KeyID: v["key_id"], KnownHostsPath: d.Server.KnownHostsPath, ProxyID: v["proxy_id"], JumpHostIDs: jumps, Tags: listValues(v["tags"])}
		if d.Server.AuthMethod != "key" {
			d.Server.KeyID = ""
		} else if d.Server.KeyID == "" {
			fields["key_id"] = m.tr.Message("ui.required")
		}
		for i := 0; ; i++ {
			p := fmt.Sprintf("forward.%d.", i)
			kind, ok := v[p+"type"]
			if !ok {
				break
			}
			def := coreclient.ForwardDefinition{Type: kind, Bind: strings.TrimSpace(v[p+"bind"]), Target: strings.TrimSpace(v[p+"target"]), Enabled: v[p+"enabled"] == "true"}
			if i < len(d.Definitions) {
				def.ID = d.Definitions[i].ID
			}
			for name, key := range validateForward(def.Type, def.Bind, def.Target) {
				fields[p+name] = m.tr.Message(key)
			}
			defs = append(defs, def)
		}
	}
	if d.Kind == coreclient.Keys {
		if v["source"] == "file" && !filepath.IsAbs(v["source_path"]) {
			fields["source_path"] = m.tr.Message("manage.absolute_path")
		}
	}
	if d.Kind == coreclient.Proxies {
		d.Proxy = coreclient.ProxyWrite{ID: d.ID, Alias: strings.TrimSpace(v["alias"]), Type: v["type"], Host: strings.TrimSpace(v["host"]), Port: port, Username: v["username"]}
	}
	if d.Kind == coreclient.Providers {
		kind := d.Provider.Type
		if !d.CredentialsOnly {
			kind = v["type"]
			d.Provider = coreclient.ProviderWrite{ID: d.ID, Alias: strings.TrimSpace(v["alias"]), Type: kind}
			if kind == "s3" {
				d.Provider.Bucket = strings.TrimSpace(v["bucket"])
				d.Provider.Key = v["key"]
				d.Provider.Region = strings.TrimSpace(v["region"])
				d.Provider.Endpoint = strings.TrimSpace(v["endpoint"])
				d.Provider.PathStyle = v["path_style"] == "true"
			} else {
				d.Provider.URL = strings.TrimSpace(v["url"])
				d.Provider.Username = v["username"]
			}
		}
		if kind == "s3" {
			if (v["access_key_id"] == "") != (v["secret_access_key"] == "") {
				fields["secret_access_key"] = m.tr.Message("manage.s3_pair")
			}
			if v["clear_s3"] == "true" && (v["access_key_id"] != "" || v["secret_access_key"] != "" || v["session_token"] != "") {
				fields["access_key_id"] = m.tr.Message("manage.clear_conflict")
			}
			if v["session_token"] != "" && v["access_key_id"] == "" {
				fields["access_key_id"] = m.tr.Message("manage.s3_pair")
			}
		}
	}
	if len(fields) > 0 {
		return m.formErrors(modal, fields)
	}
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	client, instance, modalID := m.snapshot.Client, modal.instanceID, modal.ID
	return func() tea.Msg {
		defer cancel()
		result := manageResult{ModalID: modalID, Instance: instance, ID: d.ID, Message: "manage.saved"}
		err := client.CheckInstance(ctx, instance)
		if err == nil && !d.CredentialsOnly {
			switch d.Kind {
			case coreclient.Servers:
				_, err = client.WriteServer(ctx, d.Create, d.Server)
			case coreclient.Proxies:
				_, err = client.WriteProxy(ctx, d.Create, d.Proxy)
			case coreclient.Keys:
				_, err = client.WriteKey(ctx, d.Create, coreclient.KeyWrite{ID: d.ID, Alias: strings.TrimSpace(v["alias"])})
			case coreclient.Providers:
				_, err = client.WriteProvider(ctx, d.Create, d.Provider)
			}
			result.Created = err == nil && d.Create
		}
		if err == nil && d.Kind == coreclient.Servers && !d.CredentialsOnly {
			err = client.PutForwardDefinitions(ctx, d.ID, defs)
			result.Message = "manage.server_saved"
		}
		if err == nil {
			switch d.Kind {
			case coreclient.Keys:
				material := coreclient.KeyMaterialWrite{Passphrase: v["passphrase"]}
				if v["source"] == "file" {
					material.SourcePath = v["source_path"]
				} else {
					material.PrivateKey = v["private_key"]
				}
				err = client.KeyMaterial(ctx, d.ID, material, false)
			case coreclient.Providers:
				if d.Provider.Type == "s3" {
					if v["access_key_id"] != "" || v["clear_s3"] == "true" {
						err = client.S3Credentials(ctx, d.ID, coreclient.S3CredentialsWrite{AccessKeyID: v["access_key_id"], SecretAccessKey: v["secret_access_key"], SessionToken: v["session_token"]}, v["clear_s3"] == "true")
					}
				} else if v["password"] != "" || v["clear_password"] == "true" {
					err = client.Password(ctx, d.Kind, d.ID, v["password"], v["clear_password"] == "true")
				}
			default:
				if (d.Kind != coreclient.Servers || d.CredentialsOnly || d.Server.AuthMethod == "password") && (v["password"] != "" || v["clear_password"] == "true") {
					err = client.Password(ctx, d.Kind, d.ID, v["password"], v["clear_password"] == "true")
				}
			}
		}
		result.Err = err
		return result
	}
}

func (m *Model) jumpCycle(self string, jumps []string) bool {
	graph := map[string][]string{}
	for _, s := range m.snapshot.Config.Servers {
		graph[s.ID] = s.JumpHostIDs
	}
	graph[self] = jumps
	active, done := map[string]bool{}, map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if active[id] {
			return true
		}
		if done[id] {
			return false
		}
		active[id] = true
		for _, next := range graph[id] {
			if visit(next) {
				return true
			}
		}
		active[id] = false
		done[id] = true
		return false
	}
	return visit(self)
}

func validateForward(kind, bind, target string) map[string]string {
	fields := map[string]string{}
	host, port, err := net.SplitHostPort(bind)
	n, e := strconv.Atoi(port)
	ipHost := strings.Split(host, "%")[0]
	if err != nil || e != nil || n < 0 || n > 65535 || (net.ParseIP(ipHost) == nil && host != "localhost") {
		fields["bind"] = "manage.invalid_bind"
	}
	if kind == "dynamic" {
		if target != "" {
			fields["target"] = "manage.dynamic_target"
		}
	} else {
		host, port, err = net.SplitHostPort(target)
		n, e = strconv.Atoi(port)
		if err != nil || e != nil || host == "" || n < 1 || n > 65535 {
			fields["target"] = "manage.invalid_target"
		}
	}
	return fields
}

func (m *Model) handleManageResult(msg manageResult) tea.Cmd {
	modal := m.modal
	if modal == nil || modal.ID != msg.ModalID || modal.instanceID != msg.Instance || msg.Instance != m.snapshot.Runtime.InstanceID {
		return nil
	}
	modal.Busy = false
	if modal.Form != nil {
		modal.Form.Busy = false
	}
	if msg.Created && modal.Edit != nil {
		modal.Edit.Create = false
	}
	if msg.Err != nil {
		fields := msg.Fields
		if fields == nil {
			fields = map[string]string{}
		}
		var api *coreclient.Error
		if errors.As(msg.Err, &api) {
			var details struct {
				Fields []struct {
					Field string `json:"field"`
				} `json:"fields"`
			}
			if json.Unmarshal(api.Details, &details) == nil {
				for _, f := range details.Fields {
					fields[f.Field] = m.tr.ProtocolError(api.Code)
				}
			}
		}
		code := coreclient.Code(msg.Err)
		if modal.Edit != nil && modal.Edit.Kind == coreclient.Keys {
			if code == "PASSPHRASE_REQUIRED" || (code == "INVALID_PRIVATE_KEY" && modal.Form.Values()["passphrase"] != "") {
				fields["passphrase"] = m.tr.ProtocolError(code)
			}
		}
		var cmd tea.Cmd
		if modal.Form != nil {
			cmd = m.formErrors(modal, fields)
		}
		return tea.Batch(cmd, m.notify(NoticeMsg{ID: "save", Text: m.tr.ProtocolError(code), Error: true}))
	}
	if msg.ID != "" {
		m.selectedID = msg.ID
		modal.resourceID = msg.ID
	}
	if modal.Edit != nil && modal.Edit.Kind == coreclient.Proxies && m.cancelProxyTest != nil {
		m.cancelProxyTest()
		m.removeNotice("proxy-test")
	}
	return tea.Batch(m.dismissModal(false), m.notify(NoticeMsg{ID: "save", Text: m.tr.Message(msg.Message)}), m.beginLoad(false))
}

func (m *Model) confirmResourceAction(kind coreclient.Resource, action string) tea.Cmd {
	id := m.selectedID
	alias := id
	for _, row := range m.rows() {
		if row.ID == id {
			alias = row.Alias
			break
		}
	}
	modal := &Modal{Kind: ActionConfirm, Title: m.tr.Message("manage." + action), Body: m.tr.Message("manage."+action+"_confirm") + "\n" + alias}
	client, instance := m.snapshot.Client, m.snapshot.Runtime.InstanceID
	clearDefault := false
	for _, p := range m.snapshot.Config.SyncProviders {
		if p.ID == id {
			clearDefault = p.Default
		}
	}
	modal.OnConfirm = func(string) tea.Cmd {
		modal.Busy = true
		modalID := modal.ID
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
			defer cancel()
			err := client.CheckInstance(ctx, instance)
			if err == nil {
				switch action {
				case "delete":
					err = client.DeleteResource(ctx, kind, id)
				case "material":
					err = client.KeyMaterial(ctx, id, coreclient.KeyMaterialWrite{}, true)
				case "default":
					err = client.DefaultProvider(ctx, id, clearDefault)
				}
			}
			return manageResult{ModalID: modalID, Instance: instance, Err: err, Message: "manage.saved"}
		}
	}
	return m.OpenModal(modal)
}

func (m *Model) openSyncPassword() tea.Cmd {
	clear := m.boolField("clear_password", "manage.clear_password", false)
	fields := []Field{}
	if m.snapshot.Config.Settings.SyncPasswordSet {
		fields = append(fields, clear)
	}
	fields = append(fields, conditional(m.text("password", "ui.password", "", true, true), "clear_password", "", "false"), conditional(m.text("confirm_password", "manage.confirm_password", "", true, true), "clear_password", "", "false"))
	modal := &Modal{Kind: EditForm, Title: m.tr.Message("manage.sync_password"), Form: NewForm(fields...)}
	modal.OnSubmit = func(v map[string]string) tea.Cmd {
		if v["clear_password"] != "true" && v["password"] != v["confirm_password"] {
			return m.formErrors(modal, map[string]string{"confirm_password": m.tr.Message("manage.password_mismatch")})
		}
		client, instance, id := m.snapshot.Client, modal.instanceID, modal.ID
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
			defer cancel()
			err := client.CheckInstance(ctx, instance)
			if err == nil {
				err = client.SyncPassword(ctx, v["password"], v["clear_password"] == "true")
			}
			return manageResult{ModalID: id, Instance: instance, Err: err, Message: "manage.saved"}
		}
	}
	return m.OpenModal(modal)
}
