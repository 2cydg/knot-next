package response

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONErrorHandlesInvalidAndSensitiveDetails(t *testing.T) {
	for _, value := range []any{make(chan int), func() {}, math.NaN(), map[string]any{"password": "private-sentinel", "nested": map[string]any{"token": "token-sentinel", "count": 7}}} {
		w := httptest.NewRecorder()
		JSONError(w, 400, Error{Code: "FAILED", Message: "failed", Details: map[string]any{"value": value}})
		var got ErrorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Error.Code != "FAILED" || got.Error.Details == nil {
			t.Fatalf("lost envelope: %s", w.Body.String())
		}
		for _, sentinel := range []string{"private-sentinel", "token-sentinel"} {
			if strings.Contains(w.Body.String(), sentinel) {
				t.Fatal("secret leaked")
			}
		}
	}
}
