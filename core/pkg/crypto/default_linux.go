//go:build linux

package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"knot-core/internal/paths"

	"github.com/godbus/dbus/v5"
)

const (
	ssServiceName   = "org.freedesktop.secrets"
	ssObjectPath    = "/org/freedesktop/secrets"
	ssInterface     = "org.freedesktop.Secret.Service"
	ssCollInterface = "org.freedesktop.Secret.Collection"
	ssPromptIface   = "org.freedesktop.Secret.Prompt"
	ssLoginCollPath = "/org/freedesktop/secrets/collection/login"
)

var ssItemAttributes = map[string]string{
	"service": "knot-core",
	"account": "knot-core-master-key",
}

func providerForState(layout paths.Layout, providerID string) (Provider, error) {
	switch providerID {
	case ProviderLinuxSecret:
		key, err := getSecretServiceKey(3 * time.Second)
		if err != nil {
			return nil, fmt.Errorf("crypto provider is %s but Secret Service is unavailable: %w", ProviderLinuxSecret, err)
		}
		return NewKeyProvider(ProviderLinuxSecret, key, nil), nil
	case ProviderLinuxMachine:
		return newLinuxMachineProvider(layout, nil)
	case ProviderLocal:
		return NewLocalProvider(localKeyPath(layout))
	default:
		return nil, fmt.Errorf("unknown crypto provider %q", providerID)
	}
}

func selectDefaultProvider(layout paths.Layout) (Provider, error) {
	key, err := getOrCreateSecretServiceKey(3 * time.Second)
	if err == nil {
		return NewKeyProvider(ProviderLinuxSecret, key, nil), nil
	}
	return newLinuxMachineProvider(layout, []string{"Secret Service unavailable; using local machine fallback"})
}

func newLinuxMachineProvider(layout paths.Layout, limitations []string) (Provider, error) {
	machineID, err := getMachineID()
	if err != nil {
		return nil, err
	}
	salt, err := GetSalt(layout)
	if err != nil {
		return nil, err
	}
	material := machineID + "\x00" + strconv.Itoa(os.Getuid())
	return NewKeyProvider(ProviderLinuxMachine, DeriveKey(material, salt), limitations), nil
}

func getDBusConn() (*dbus.Conn, error) {
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		fallback := fmt.Sprintf("unix:path=/run/user/%d/bus", os.Getuid())
		if _, err := os.Stat(strings.TrimPrefix(fallback, "unix:path=")); err == nil {
			addr = fallback
		}
	}
	if addr == "" {
		return nil, fmt.Errorf("no D-Bus session address found")
	}
	return dbus.Connect(addr)
}

func getSecretServiceKey(timeout time.Duration) ([]byte, error) {
	return secretServiceKey(timeout, false)
}

func getOrCreateSecretServiceKey(timeout time.Duration) ([]byte, error) {
	return secretServiceKey(timeout, true)
}

func secretServiceKey(timeout time.Duration, allowCreate bool) ([]byte, error) {
	conn, err := getDBusConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	obj := conn.Object(ssServiceName, ssObjectPath)
	var sessionPath dbus.ObjectPath
	var outVariant dbus.Variant
	if err := obj.CallWithContext(ctx, ssInterface+".OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&outVariant, &sessionPath); err != nil {
		return nil, fmt.Errorf("OpenSession failed: %w", err)
	}

	var unlocked []dbus.ObjectPath
	var locked []dbus.ObjectPath
	if err := obj.CallWithContext(ctx, ssInterface+".SearchItems", 0, ssItemAttributes).Store(&unlocked, &locked); err != nil {
		return nil, fmt.Errorf("SearchItems failed: %w", err)
	}
	itemPath := dbus.ObjectPath("")
	if len(unlocked) > 0 {
		itemPath = unlocked[0]
	} else if len(locked) > 0 {
		paths, err := unlockSecretServiceItems(ctx, conn, obj, locked)
		if err != nil {
			return nil, fmt.Errorf("Unlock failed: %w", err)
		}
		if len(paths) > 0 {
			itemPath = paths[0]
		}
	}
	if validSecretServicePath(itemPath) {
		key, err := readSecretServiceItem(ctx, conn, itemPath, sessionPath)
		if err == nil && len(key) == 32 {
			return key, nil
		}
	}
	if !allowCreate {
		return nil, fmt.Errorf("Secret Service item not found")
	}
	return createSecretServiceKey(ctx, conn, obj, sessionPath)
}

func readSecretServiceItem(ctx context.Context, conn *dbus.Conn, itemPath dbus.ObjectPath, sessionPath dbus.ObjectPath) ([]byte, error) {
	type secret struct {
		Session     dbus.ObjectPath
		Parameters  []byte
		Value       []byte
		ContentType string
	}
	var out secret
	if err := conn.Object(ssServiceName, itemPath).CallWithContext(ctx, "org.freedesktop.Secret.Item.GetSecret", 0, sessionPath).Store(&out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(string(out.Value)))
}

func createSecretServiceKey(ctx context.Context, conn *dbus.Conn, obj dbus.BusObject, sessionPath dbus.ObjectPath) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	type secretInput struct {
		Session     dbus.ObjectPath
		Parameters  []byte
		Value       []byte
		ContentType string
	}
	properties := map[string]dbus.Variant{
		"org.freedesktop.Secret.Item.Label":      dbus.MakeVariant("Knot Core Master Key"),
		"org.freedesktop.Secret.Item.Attributes": dbus.MakeVariant(ssItemAttributes),
	}
	input := secretInput{
		Session:     sessionPath,
		Parameters:  []byte{},
		Value:       []byte(base64.StdEncoding.EncodeToString(key)),
		ContentType: "text/plain",
	}
	collections, err := secretServiceCollections(ctx, obj)
	if err != nil {
		return nil, err
	}
	for _, collection := range collections {
		if _, err := createSecretServiceItem(ctx, conn, collection, properties, input); err == nil {
			return key, nil
		}
	}
	return nil, fmt.Errorf("CreateItem failed: no usable Secret Service collection")
}

func unlockSecretServiceItems(ctx context.Context, conn *dbus.Conn, obj dbus.BusObject, locked []dbus.ObjectPath) ([]dbus.ObjectPath, error) {
	var unlocked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	if err := obj.CallWithContext(ctx, ssInterface+".Unlock", 0, locked).Store(&unlocked, &prompt); err != nil {
		return nil, err
	}
	if len(unlocked) > 0 || !validSecretServicePath(prompt) {
		return unlocked, nil
	}
	result, err := completeSecretServicePrompt(ctx, conn, prompt)
	if err != nil {
		return nil, err
	}
	paths, ok := result.Value().([]dbus.ObjectPath)
	if !ok {
		return nil, fmt.Errorf("prompt result has unexpected type %T", result.Value())
	}
	return paths, nil
}

func createSecretServiceItem(ctx context.Context, conn *dbus.Conn, collection dbus.ObjectPath, properties map[string]dbus.Variant, input any) (dbus.ObjectPath, error) {
	var item dbus.ObjectPath
	var prompt dbus.ObjectPath
	if err := conn.Object(ssServiceName, collection).CallWithContext(ctx, ssCollInterface+".CreateItem", 0, properties, input, true).Store(&item, &prompt); err != nil {
		return "", err
	}
	if validSecretServicePath(item) {
		return item, nil
	}
	if !validSecretServicePath(prompt) {
		return "", fmt.Errorf("CreateItem returned no item and no prompt")
	}
	result, err := completeSecretServicePrompt(ctx, conn, prompt)
	if err != nil {
		return "", err
	}
	item, ok := result.Value().(dbus.ObjectPath)
	if !ok {
		return "", fmt.Errorf("prompt result has unexpected type %T", result.Value())
	}
	return item, nil
}

func completeSecretServicePrompt(ctx context.Context, conn *dbus.Conn, prompt dbus.ObjectPath) (dbus.Variant, error) {
	sigCh := make(chan *dbus.Signal, 4)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	match := []dbus.MatchOption{
		dbus.WithMatchObjectPath(prompt),
		dbus.WithMatchInterface(ssPromptIface),
		dbus.WithMatchMember("Completed"),
	}
	if err := conn.AddMatchSignalContext(ctx, match...); err != nil {
		return dbus.Variant{}, err
	}
	defer func() {
		_ = conn.RemoveMatchSignalContext(context.Background(), match...)
	}()
	if err := conn.Object(ssServiceName, prompt).CallWithContext(ctx, ssPromptIface+".Prompt", 0, "").Store(); err != nil {
		return dbus.Variant{}, err
	}
	for {
		select {
		case <-ctx.Done():
			return dbus.Variant{}, ctx.Err()
		case sig := <-sigCh:
			if sig == nil || sig.Path != prompt || sig.Name != ssPromptIface+".Completed" || len(sig.Body) != 2 {
				continue
			}
			dismissed, ok := sig.Body[0].(bool)
			if !ok {
				return dbus.Variant{}, fmt.Errorf("prompt dismissed field has unexpected type %T", sig.Body[0])
			}
			if dismissed {
				return dbus.Variant{}, fmt.Errorf("prompt dismissed")
			}
			result, ok := sig.Body[1].(dbus.Variant)
			if !ok {
				return dbus.Variant{}, fmt.Errorf("prompt result has unexpected type %T", sig.Body[1])
			}
			return result, nil
		}
	}
}

func secretServiceCollections(ctx context.Context, obj dbus.BusObject) ([]dbus.ObjectPath, error) {
	var defaultPath dbus.ObjectPath
	_ = obj.CallWithContext(ctx, ssInterface+".ReadAlias", 0, "default").Store(&defaultPath)
	var variant dbus.Variant
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, ssInterface, "Collections").Store(&variant); err != nil {
		if validSecretServicePath(defaultPath) {
			return []dbus.ObjectPath{defaultPath, ssLoginCollPath}, nil
		}
		return nil, err
	}
	collections, _ := variant.Value().([]dbus.ObjectPath)
	seen := map[dbus.ObjectPath]bool{}
	out := make([]dbus.ObjectPath, 0, len(collections)+2)
	add := func(path dbus.ObjectPath) {
		if validSecretServicePath(path) && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	add(defaultPath)
	for _, path := range collections {
		add(path)
	}
	add(ssLoginCollPath)
	return out, nil
}

func validSecretServicePath(path dbus.ObjectPath) bool {
	return path != "" && path != "/"
}

func getMachineID() (string, error) {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		raw, err := os.ReadFile(path)
		if err == nil {
			id := strings.TrimSpace(string(raw))
			if id != "" {
				return id, nil
			}
		}
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "", fmt.Errorf("could not resolve machine id")
	}
	return hostname, nil
}
