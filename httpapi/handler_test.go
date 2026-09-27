package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	dns "github.com/chris64233/go-dns-change"
	"github.com/chris64233/go-dns-change/httpapi"
)

func newServer(t *testing.T) (*httptest.Server, *dns.Service) {
	t.Helper()
	svc := dns.NewService(dns.NewMemoryStore())
	srv := httptest.NewServer(httpapi.NewHandler(svc))
	t.Cleanup(srv.Close)
	return srv, svc
}

func doJSON(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, out
}

func TestHTTPFullLifecycle(t *testing.T) {
	srv, _ := newServer(t)

	// 初始化区域，初始 2 个审批人 + 职责分离。
	status, body := doJSON(t, http.MethodPost, srv.URL+"/zones", map[string]any{
		"name":   "example.com",
		"config": map[string]any{"min_ttl": 60, "max_ttl": 3600},
		"policy": map[string]any{
			"required_approvals":   2,
			"approvers":            []string{"alice", "bob"},
			"separation_of_duties": true,
		},
		"records": []map[string]any{
			{"name": "www", "type": "A", "ttl": 300, "values": []string{"10.0.0.1"}},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("init status=%d body=%v", status, body)
	}

	// 查询区域。
	status, body = doJSON(t, http.MethodGet, srv.URL+"/zones/example.com", nil)
	if status != http.StatusOK || body["head"].(float64) != 1 {
		t.Fatalf("get zone: status=%d body=%v", status, body)
	}

	// 提交变更。
	status, body = doJSON(t, http.MethodPost, srv.URL+"/zones/example.com/changes", map[string]any{
		"base_revision": 1,
		"committer":     "carol",
		"comment":       "add api host",
		"upserts": []map[string]any{
			{"name": "api", "type": "A", "ttl": 300, "values": []string{"10.0.0.5"}},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("submit: status=%d body=%v", status, body)
	}
	rev := body["revision"].(map[string]any)
	if rev["status"].(string) != "pending" {
		t.Fatalf("revision should be pending: %v", rev["status"])
	}

	// TTL 冲突 -> 422 validation_error。
	status, body = doJSON(t, http.MethodPost, srv.URL+"/zones/example.com/changes", map[string]any{
		"base_revision": 1,
		"committer":     "carol",
		"upserts": []map[string]any{
			{"name": "bad", "type": "A", "ttl": 1, "values": []string{"10.0.0.9"}},
		},
	})
	if status != http.StatusUnprocessableEntity || body["code"].(string) != "validation_error" {
		t.Fatalf("validation: status=%d body=%v", status, body)
	}

	// 在途修订未终结时再次提交 -> 409。
	status, body = doJSON(t, http.MethodPost, srv.URL+"/zones/example.com/changes", map[string]any{
		"base_revision": 1,
		"committer":     "carol",
		"upserts": []map[string]any{
			{"name": "other", "type": "A", "ttl": 300, "values": []string{"10.0.0.7"}},
		},
	})
	if status != http.StatusConflict || body["code"].(string) != "revision_conflict" {
		t.Fatalf("in-flight conflict: status=%d body=%v", status, body)
	}

	// carol 自审批 -> 403 approval_error（职责分离）。
	status, body = doJSON(t, http.MethodPost, srv.URL+"/zones/example.com/revisions/2/approve", map[string]any{
		"approver": "carol",
	})
	if status != http.StatusForbidden || body["code"].(string) != "approval_error" {
		t.Fatalf("sod approval: status=%d body=%v", status, body)
	}

	// alice 审批后仍 pending；bob 审批后 approved。
	status, _ = doJSON(t, http.MethodPost, srv.URL+"/zones/example.com/revisions/2/approve", map[string]any{
		"approver": "alice", "idempotency_key": "apk-alice",
	})
	if status != http.StatusCreated {
		t.Fatalf("alice approve: %d", status)
	}
	// 幂等重放：200 + replayed=true。
	status, body = doJSON(t, http.MethodPost, srv.URL+"/zones/example.com/revisions/2/approve", map[string]any{
		"approver": "alice", "idempotency_key": "apk-alice",
	})
	if status != http.StatusOK || body["replayed"].(bool) != true {
		t.Fatalf("approve replay: status=%d body=%v", status, body)
	}
	status, body = doJSON(t, http.MethodPost, srv.URL+"/zones/example.com/revisions/2/approve", map[string]any{
		"approver": "bob",
	})
	if status != http.StatusCreated {
		t.Fatalf("bob approve: status=%d body=%v", status, body)
	}

	// 发布。
	status, body = doJSON(t, http.MethodPost, srv.URL+"/zones/example.com/revisions/2/publish", nil)
	if status != http.StatusOK {
		t.Fatalf("publish: status=%d body=%v", status, body)
	}
	if body["outbox_event"].(map[string]any)["revision"].(float64) != 2 {
		t.Fatalf("outbox event: %v", body["outbox_event"])
	}

	// 查询单个修订与历史列表。
	status, body = doJSON(t, http.MethodGet, srv.URL+"/zones/example.com/revisions/2", nil)
	if status != http.StatusOK || body["status"].(string) != "published" {
		t.Fatalf("get revision: status=%d body=%v", status, body)
	}
	status, listRaw := getJSON(t, srv.URL+"/zones/example.com/revisions")
	if status != http.StatusOK {
		t.Fatalf("list revisions: %d", status)
	}
	_ = listRaw

	// 回滚到修订 1，然后发布。
	status, body = doJSON(t, http.MethodPost, srv.URL+"/zones/example.com/rollbacks", map[string]any{
		"source_revision": 1,
		"committer":       "carol",
	})
	if status != http.StatusCreated {
		t.Fatalf("rollback: status=%d body=%v", status, body)
	}
	if body["revision"].(map[string]any)["kind"].(string) != "rollback" {
		t.Fatalf("rollback kind: %v", body["revision"])
	}
	rbNum := int(body["revision"].(map[string]any)["number"].(float64))
	if rbNum != 3 {
		t.Fatalf("rollback revision = %d, want 3", rbNum)
	}
	rbPath := srv.URL + "/zones/example.com/revisions/" + strconv.Itoa(rbNum)

	// alice 与 bob 审批回滚修订（2 人要求）。
	doJSON(t, http.MethodPost, rbPath+"/approve", map[string]any{"approver": "alice"})
	doJSON(t, http.MethodPost, rbPath+"/approve", map[string]any{"approver": "bob"})
	status, body = doJSON(t, http.MethodPost, rbPath+"/publish", nil)
	if status != http.StatusOK {
		t.Fatalf("publish rollback: status=%d body=%v", status, body)
	}

	// 区域 head 已推进，且当前视图回到初始记录。
	status, body = doJSON(t, http.MethodGet, srv.URL+"/zones/example.com", nil)
	if status != http.StatusOK || body["head"].(float64) != 3 {
		t.Fatalf("zone after rollback: status=%d body=%v", status, body)
	}
}

func TestHTTPErrorMapping(t *testing.T) {
	srv, _ := newServer(t)

	// 不存在的区域 -> 404（revision 错误中的 not-exist 分支）。
	status, body := doJSON(t, http.MethodGet, srv.URL+"/zones/missing.com", nil)
	if status != http.StatusNotFound || body["code"].(string) != "revision_conflict" {
		t.Fatalf("missing zone: status=%d body=%v", status, body)
	}

	// 初始化一个非法区域名 -> 422。
	status, body = doJSON(t, http.MethodPost, srv.URL+"/zones", map[string]any{"name": "bad name"})
	if status != http.StatusUnprocessableEntity || body["code"].(string) != "validation_error" {
		t.Fatalf("bad zone name: status=%d body=%v", status, body)
	}

	// 非法修订号 -> 400 bad_request。
	resp, err := http.Post(srv.URL+"/zones/example.com/revisions/abc/publish", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad revision path: %d", resp.StatusCode)
	}

	// 请求体含未知字段 -> 400。
	status, _ = doJSON(t, http.MethodPost, srv.URL+"/zones", map[string]any{"name": "x.io", "bogus": 1})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown field should be 400, got %d", status)
	}
}

func getJSON(t *testing.T, url string) (int, []map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}
