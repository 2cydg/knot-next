package integration

import (
	"bytes"
	protocolsftp "github.com/pkg/sftp"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostKeyExpiryThroughPublicAPIKeepsTrustUnchanged(t *testing.T) {
	c, _ := newSFTPAPIWithDeadline(t, protocolsftp.InMemHandler(), 300*time.Millisecond)
	type challengeJSON struct {
		ID          string `json:"challenge_id"`
		Outcome     string `json:"outcome"`
		Pending     bool   `json:"pending"`
		FailureCode string `json:"failure_code"`
	}
	type resourceJSON struct {
		ID            string         `json:"id"`
		State         string         `json:"state"`
		FailureCode   string         `json:"failure_code"`
		LastChallenge *challengeJSON `json:"last_challenge"`
	}
	var resources []struct {
		route     string
		id        string
		challenge challengeJSON
	}
	for _, route := range []string{"/v1/sessions", "/v1/sftp"} {
		var resource resourceJSON
		c.call(t, http.MethodPost, route, map[string]any{"server_ref": c.serverRef}, 201, &resource)
		deadline := time.Now().Add(2 * time.Second)
		var challenge challengeJSON
		for {
			c.call(t, http.MethodGet, route+"/"+resource.ID+"/challenges/host-key", nil, 200, &challenge)
			if challenge.Pending {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("host challenge missing")
			}
			time.Sleep(time.Millisecond)
		}
		resources = append(resources, struct {
			route     string
			id        string
			challenge challengeJSON
		}{route, resource.ID, challenge})
	}
	var summary struct {
		Metadata struct {
			ConfigPath string `json:"config_path"`
		} `json:"metadata"`
	}
	c.call(t, http.MethodGet, "/v1/config", nil, 200, &summary)
	trustPath := filepath.Join(filepath.Dir(summary.Metadata.ConfigPath), "known_hosts")
	before, err := os.ReadFile(trustPath)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for _, entry := range resources {
		var resource resourceJSON
		for {
			c.call(t, http.MethodGet, entry.route+"/"+entry.id, nil, 200, &resource)
			if resource.State == "failed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("challenge did not expire")
			}
			time.Sleep(100 * time.Millisecond)
		}
		if resource.FailureCode != "HOST_KEY_CHALLENGE_EXPIRED" || resource.LastChallenge == nil || resource.LastChallenge.ID != entry.challenge.ID || resource.LastChallenge.Outcome != "expired" {
			t.Fatalf("expiry snapshot: %+v", resource)
		}
		requestBody := map[string]any{"challenge_id": entry.challenge.ID, "accept": true}
		c.call(t, http.MethodPost, entry.route+"/"+entry.id+"/challenges/host-key", requestBody, 409, nil)
		// Read the actual envelope code, not only HTTP status.
		req, err := http.NewRequest(http.MethodPost, c.baseURL+entry.route+"/"+entry.id+"/challenges/host-key", bytes.NewBufferString(`{"challenge_id":"`+entry.challenge.ID+`","accept":true}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte(`"code":"HOST_KEY_CHALLENGE_EXPIRED"`)) {
			t.Fatalf("expiry envelope: %s", data)
		}
	}
	after, err := os.ReadFile(trustPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("late trust modified known_hosts")
	}
}
