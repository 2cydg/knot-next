package tui

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"knot-cli/internal/entry"
)

func (m *Model) presence(value bool) string {
	if value {
		return m.tr.Message("ui.set")
	}
	return m.tr.Message("ui.unset")
}
func (m *Model) boolean(value bool) string {
	if value {
		return m.tr.Message("ui.yes")
	}
	return m.tr.Message("ui.no")
}
func (m *Model) none(value string) string {
	if value == "" {
		return m.tr.Message("ui.none")
	}
	return value
}
func address(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }
func (m *Model) references(id string, key bool) string {
	var names []string
	for _, s := range m.snapshot.Config.Servers {
		if (key && s.KeyID == id) || (!key && s.ProxyID == id) {
			names = append(names, s.Alias)
		}
	}
	return m.none(strings.Join(names, ", "))
}
func (m *Model) allRows() []row {
	var rows []row
	config := m.snapshot.Config
	switch m.page {
	case entry.Servers:
		for _, s := range config.Servers {
			target := s.User + "@" + address(s.Host, s.Port)
			recent := m.tr.Message("ui.no_record")
			r := row{ID: s.ID, Alias: s.Alias}
			if s.LastUsed != nil {
				r.LastUsed = *s.LastUsed
				recent = s.LastUsed.Local().Format("2006-01-02 15:04")
			}
			proxy := ""
			for _, p := range config.Proxies {
				if p.ID == s.ProxyID {
					proxy = p.Alias
				}
			}
			var jumps []string
			for _, id := range s.JumpHostIDs {
				name := id
				for _, jump := range config.Servers {
					if jump.ID == id {
						name = jump.Alias
					}
				}
				jumps = append(jumps, name)
			}
			r.Cells = []string{s.Alias, target, strings.Join(s.Tags, ", "), recent}
			r.Details = [][2]string{{"target", target}, {"auth_method", m.auth(s.AuthMethod)}, {"proxy", m.none(proxy)}, {"jumps", m.none(strings.Join(jumps, " → "))}, {"tags", m.none(strings.Join(s.Tags, ", "))}, {"recent", recent}}
			if s.AuthMethod == "password" {
				r.Details = append(r.Details, [2]string{"password", m.presence(s.PasswordSet)})
			}
			if s.AuthMethod == "key" {
				key := ""
				for _, k := range config.Keys {
					if k.ID == s.KeyID {
						key = k.Alias
					}
				}
				r.Details = append(r.Details, [2]string{"auth.key", m.none(key)})
			}
			rows = append(rows, r)
		}
	case entry.Keys:
		for _, k := range config.Keys {
			typeName, bits := k.Type, strconv.Itoa(k.Length)
			if typeName == "" {
				typeName = m.tr.Message("ui.unverified")
			}
			if k.Length == 0 {
				bits = m.tr.Message("ui.unverified")
			}
			rows = append(rows, row{ID: k.ID, Alias: k.Alias, Cells: []string{k.Alias, typeName, bits}, Details: [][2]string{{"type", typeName}, {"bits", bits}, {"references", m.references(k.ID, true)}, {"material", m.presence(k.PrivateKeySet)}, {"fingerprint", m.none(k.Fingerprint)}, {"encrypted", m.boolean(k.Encrypted)}}})
		}
	case entry.Proxies:
		for _, p := range config.Proxies {
			rows = append(rows, row{ID: p.ID, Alias: p.Alias, Cells: []string{p.Alias, strings.ToUpper(p.Type), address(p.Host, p.Port), m.none(p.Username)}, Details: [][2]string{{"type", strings.ToUpper(p.Type)}, {"address", address(p.Host, p.Port)}, {"username", m.none(p.Username)}, {"references", m.references(p.ID, false)}, {"password", m.presence(p.PasswordSet)}}})
		}
	case entry.Sync:
		if !config.Settings.SyncPasswordSet {
			return nil
		}
		for _, s := range config.SyncProviders {
			typeName := s.Type
			location := s.URL
			credentials := s.PasswordSet
			details := [][2]string{{"address", s.URL}, {"username", m.none(s.Username)}}
			if s.Type == "s3" {
				typeName = "S3"
				location = s.Bucket + "/" + s.Key
				credentials = s.AccessKeyIDSet && s.SecretAccessKeySet
				details = [][2]string{{"bucket", s.Bucket}, {"object_key", s.Key}, {"region", s.Region}, {"endpoint", m.none(s.Endpoint)}, {"path_style", m.boolean(s.PathStyle)}}
			} else if s.Type == "webdav" {
				typeName = "WebDAV"
			}
			details = append([][2]string{{"type", typeName}}, details...)
			details = append(details, [2]string{"credentials", m.presence(credentials)}, [2]string{"default", m.boolean(s.Default)})
			rows = append(rows, row{ID: s.ID, Alias: s.Alias, Cells: []string{s.Alias, typeName, location, m.presence(credentials), m.boolean(s.Default)}, Details: details})
		}
	}
	return rows
}
func (m *Model) auth(method string) string {
	switch method {
	case "agent", "key", "password":
		return m.tr.Message("ui.auth." + method)
	}
	return fmt.Sprintf("%s (%s)", m.tr.Message("ui.unknown"), method)
}
