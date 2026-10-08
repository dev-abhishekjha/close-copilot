package frappe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// docName is the last path segment of /api/resource/<doctype>/<name>.
func docName(r *http.Request) string {
	return r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
}

func TestGetMany(t *testing.T) {
	type doc struct {
		Name     string `json:"name"`
		Accounts []struct {
			Account string      `json:"account"`
			Debit   json.Number `json:"debit"`
		} `json:"accounts"`
	}

	t.Run("bounded concurrency and input order", func(t *testing.T) {
		var inFlight, peak atomic.Int32
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			name := docName(r)
			writeRaw(w, http.StatusOK, fmt.Sprintf(`{"data":{"name":%q,"accounts":[{"account":"Cash - STPL","debit":100.25}]}}`, name))
		}))
		names := make([]string, 25)
		for i := range names {
			names[i] = fmt.Sprintf("ACC-JV-2026-%05d", 25-i) // not sorted, to prove order is kept
		}
		docs, err := GetMany[doc](t.Context(), c, "Journal Entry", names)
		if err != nil {
			t.Fatal(err)
		}
		if len(docs) != len(names) {
			t.Fatalf("got %d docs, want %d", len(docs), len(names))
		}
		for i, d := range docs {
			if d.Name != names[i] {
				t.Errorf("docs[%d] = %s, want %s", i, d.Name, names[i])
			}
			if len(d.Accounts) != 1 || d.Accounts[0].Debit.String() != "100.25" {
				t.Errorf("docs[%d] child table = %+v", i, d.Accounts)
			}
		}
		if p := peak.Load(); p > MaxInFlight || p < 2 {
			t.Errorf("peak concurrency = %d, want 2..%d", p, MaxInFlight)
		}
		if MaxInFlight != 4 {
			t.Errorf("MaxInFlight = %d, want 4", MaxInFlight)
		}
	})

	t.Run("first error cancels the rest", func(t *testing.T) {
		var started atomic.Int32
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started.Add(1)
			if docName(r) == "bad" {
				time.Sleep(20 * time.Millisecond) // let the others start
				writeRaw(w, http.StatusExpectationFailed, `{"exc_type":"ValidationError","exception":"bad doc"}`)
				return
			}
			select {
			case <-r.Context().Done(): // the client gave up
			case <-time.After(10 * time.Second):
				writeRaw(w, http.StatusOK, `{"data":{"name":"slow"}}`)
			}
		}))
		names := []string{"a", "bad"}
		for i := range 38 {
			names = append(names, fmt.Sprintf("n%d", i))
		}
		begin := time.Now()
		_, err := GetMany[doc](t.Context(), c, "Journal Entry", names)
		if !IsValidation(err) {
			t.Fatalf("err = %v, want the validation error (not a cancellation)", err)
		}
		if el := time.Since(begin); el > 5*time.Second {
			t.Errorf("GetMany took %v: the error did not cancel the rest", el)
		}
		if s := started.Load(); s >= int32(len(names)) {
			t.Errorf("all %d requests started; want the error to stop the queue", s)
		}
	})

	t.Run("empty", func(t *testing.T) {
		c, _ := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("unexpected request")
		}))
		docs, err := GetMany[doc](t.Context(), c, "Journal Entry", nil)
		if err != nil || len(docs) != 0 {
			t.Errorf("docs = %v, err = %v", docs, err)
		}
	})

	t.Run("cancelled parent context", func(t *testing.T) {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeRaw(w, http.StatusOK, `{"data":{"name":"x"}}`)
		}))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := GetMany[doc](ctx, c, "Journal Entry", []string{"a", "b"}); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

// recorded is one request the fake saw.
type recorded struct {
	Method, Path, RawPath, Query, ContentType string
	Body                                      map[string]any
}

func recorder(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (http.Handler, func() []recorded) {
	t.Helper()
	var (
		mu  sync.Mutex
		got []recorded
	)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{Method: r.Method, Path: r.URL.Path, RawPath: r.URL.EscapedPath(), Query: r.URL.RawQuery, ContentType: r.Header.Get("Content-Type")}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			if err := decodeJSON(b, &rec.Body); err != nil {
				t.Errorf("request body is not JSON: %s", b)
			}
		}
		mu.Lock()
		got = append(got, rec)
		mu.Unlock()
		respond(w, r)
	})
	return h, func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), got...)
	}
}

func TestWrites(t *testing.T) {
	const je = "ACC-JV-2026-00001"
	h, requests := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/resource/Journal Entry":
			writeRaw(w, http.StatusOK, `{"data":{"name":"`+je+`","docstatus":0,"total_debit":1.00}}`)
		case r.Method == http.MethodPut:
			writeRaw(w, http.StatusOK, `{"data":{"name":"`+je+`","user_remark":"changed"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/resource/Journal Entry/"+je:
			writeRaw(w, http.StatusOK, `{"data":{"name":"`+je+`","docstatus":0,"modified":"2026-10-08 10:00:00.000001","accounts":[{"account":"Cash - STPL","credit_in_account_currency":1.00}]}}`)
		case r.URL.Path == "/api/method/frappe.client.submit":
			writeRaw(w, http.StatusOK, `{"message":{"name":"`+je+`","docstatus":1}}`)
		case r.URL.Path == "/api/method/frappe.client.cancel":
			writeRaw(w, http.StatusOK, `{"message":{"name":"`+je+`","docstatus":2}}`)
		case r.Method == http.MethodDelete:
			writeRaw(w, http.StatusAccepted, `{"data":"ok"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	c, _ := newTestClient(t, h)
	ctx := t.Context()

	doc := map[string]any{
		"voucher_type": "Journal Entry",
		"accounts":     []map[string]any{{"account": "Cash - STPL", "credit_in_account_currency": json.Number("1.00")}},
	}
	ins, err := Insert(ctx, c, "Journal Entry", doc)
	if err != nil {
		t.Fatal(err)
	}
	if ins["name"] != je || ins["total_debit"] != json.Number("1.00") {
		t.Errorf("Insert = %v, want name and total_debit as json.Number", ins)
	}
	if _, err := Update(ctx, c, "Journal Entry", je, map[string]any{"user_remark": "changed"}); err != nil {
		t.Fatal(err)
	}
	sub, err := Submit(ctx, c, "Journal Entry", je)
	if err != nil {
		t.Fatal(err)
	}
	if sub["docstatus"] != json.Number("1") {
		t.Errorf("Submit docstatus = %v", sub["docstatus"])
	}
	can, err := Cancel(ctx, c, "Journal Entry", je)
	if err != nil {
		t.Fatal(err)
	}
	if can["docstatus"] != json.Number("2") {
		t.Errorf("Cancel docstatus = %v", can["docstatus"])
	}
	if err := Delete(ctx, c, "Journal Entry", je); err != nil {
		t.Fatal(err)
	}

	got := requests()
	want := []struct{ method, path string }{
		{http.MethodPost, "/api/resource/Journal Entry"},
		{http.MethodPut, "/api/resource/Journal Entry/" + je},
		{http.MethodGet, "/api/resource/Journal Entry/" + je},
		{http.MethodPost, "/api/method/frappe.client.submit"},
		{http.MethodPost, "/api/method/frappe.client.cancel"},
		{http.MethodDelete, "/api/resource/Journal Entry/" + je},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d requests, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Method != w.method || got[i].Path != w.path {
			t.Errorf("request %d = %s %s, want %s %s", i, got[i].Method, got[i].Path, w.method, w.path)
		}
		if (w.method == http.MethodPost || w.method == http.MethodPut) && got[i].ContentType != "application/json" {
			t.Errorf("request %d Content-Type = %q", i, got[i].ContentType)
		}
	}
	if got[0].Body["accounts"].([]any)[0].(map[string]any)["credit_in_account_currency"] != json.Number("1.00") {
		t.Errorf("Insert body lost the exact amount: %v", got[0].Body)
	}
	// Submit posts the whole latest document, modified timestamp and
	// doctype included, so Frappe's timestamp check passes.
	sent, ok := got[3].Body["doc"].(map[string]any)
	if !ok || sent["doctype"] != "Journal Entry" || sent["modified"] != "2026-10-08 10:00:00.000001" || sent["name"] != je {
		t.Errorf("submit body = %v", got[3].Body)
	}
	if got[4].Body["doctype"] != "Journal Entry" || got[4].Body["name"] != je {
		t.Errorf("cancel body = %v", got[4].Body)
	}
}

func TestCall(t *testing.T) {
	h, requests := recorder(t, func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, http.StatusOK, `{"message":{"count":3,"amount":12.30}}`)
	})
	c, _ := newTestClient(t, h)
	type result struct {
		Count  int         `json:"count"`
		Amount json.Number `json:"amount"`
	}
	res, err := Call[result](t.Context(), c, http.MethodGet, "frappe.client.get_count", map[string]any{
		"doctype": "Journal Entry",
		"filters": [][]any{{"docstatus", "=", 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 3 || res.Amount.String() != "12.30" {
		t.Errorf("result = %+v", res)
	}
	got := requests()[0]
	if got.Method != http.MethodGet || got.Path != "/api/method/frappe.client.get_count" {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
	if got.Query != "doctype=Journal+Entry&filters=%5B%5B%22docstatus%22%2C%22%3D%22%2C1%5D%5D" {
		t.Errorf("query = %s", got.Query)
	}

	if _, err := Call[result](t.Context(), c, http.MethodPost, "frappe.client.get_count", map[string]any{"doctype": "Journal Entry"}); err != nil {
		t.Fatal(err)
	}
	if got := requests()[1]; got.Method != http.MethodPost || got.Body["doctype"] != "Journal Entry" || got.Query != "" {
		t.Errorf("POST request = %+v", got)
	}

	bad := []struct{ method, path string }{
		{http.MethodGet, "frappe"},
		{http.MethodGet, "frappe.client/../x"},
		{http.MethodGet, "../frappe.client.get"},
		{http.MethodGet, "frappe.client.get?x=1"},
		{http.MethodDelete, "frappe.client.delete"},
		{http.MethodPut, "frappe.client.submit"},
	}
	for _, b := range bad {
		if _, err := Call[any](t.Context(), c, b.method, b.path, nil); err == nil {
			t.Errorf("Call(%s, %q): want an error", b.method, b.path)
		}
	}
	if n := len(requests()); n != 2 {
		t.Errorf("rejected calls still sent requests: %d total", n)
	}
}

func TestResourcePath(t *testing.T) {
	h, requests := recorder(t, func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, http.StatusOK, `{"data":{"name":"x"}}`)
	})
	c, _ := newTestClient(t, h)
	if _, err := Get[map[string]any](t.Context(), c, "Account", "Bank / Cash - STPL"); err != nil {
		t.Fatal(err)
	}
	if got := requests()[0].RawPath; got != "/api/resource/Account/Bank%20%2F%20Cash%20-%20STPL" {
		t.Errorf("escaped path = %s", got)
	}
	for _, name := range []string{"", ".", "..", "a\nb"} {
		if _, err := Get[map[string]any](t.Context(), c, "Account", name); !errors.Is(err, errBadName) {
			t.Errorf("Get(%q) err = %v, want errBadName", name, err)
		}
	}
	if err := Delete(t.Context(), c, "Account", ".."); !errors.Is(err, errBadName) {
		t.Errorf("Delete(..) err = %v", err)
	}
}
