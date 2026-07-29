package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const issuesFixture = `{"data":{"issues":{"nodes":[
  {"id":"i1","identifier":"ENG-1","title":"Crash on login","description":"stack trace",
   "priority":2,"url":"https://linear.app/x/issue/ENG-1","createdAt":"2026-07-10T10:00:00Z","updatedAt":"2026-07-11T10:00:00Z",
   "state":{"name":"In Progress"},"assignee":{"name":"Ana"},"project":{"id":"p1","name":"Core"},"team":{"id":"t1","name":"Eng"},
   "labels":{"nodes":[{"name":"bug"}]}}
],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`

func TestIssuesParsesNodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(issuesFixture))
	}))
	defer srv.Close()

	c := New(srv.URL, "lin_test", srv.Client())
	issues, err := c.Issues(context.Background(), time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("want 1 issue, got %d", len(issues))
	}
	got := issues[0]
	if got.Identifier != "ENG-1" || got.State != "In Progress" || got.Assignee != "Ana" {
		t.Fatalf("bad mapping: %+v", got)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "bug" || !strings.HasPrefix(got.URL, "https://") {
		t.Fatalf("bad labels/url: %+v", got)
	}
}

// TestIssuesPaginates verifies the client follows pageInfo across multiple pages.
func TestIssuesPaginates(t *testing.T) {
	page1 := `{"data":{"issues":{"nodes":[
	  {"id":"i1","identifier":"ENG-1"},{"id":"i2","identifier":"ENG-2"}
	],"pageInfo":{"hasNextPage":true,"endCursor":"CURSOR1"}}}}`
	page2 := `{"data":{"issues":{"nodes":[
	  {"id":"i3","identifier":"ENG-3"}
	],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`

	var gotCursor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		if after, ok := req.Variables["after"].(string); ok && after == "CURSOR1" {
			gotCursor = after
			w.Write([]byte(page2))
			return
		}
		w.Write([]byte(page1))
	}))
	defer srv.Close()

	c := New(srv.URL, "lin_test", srv.Client())
	issues, err := c.Issues(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 3 {
		t.Fatalf("want 3 issues across 2 pages, got %d", len(issues))
	}
	if gotCursor != "CURSOR1" {
		t.Fatalf("second page did not send endCursor, got %q", gotCursor)
	}
}

func TestProjectNamesPaginates(t *testing.T) {
	page1 := `{"data":{"projects":{"nodes":[{"id":"p1","name":"Core"}],"pageInfo":{"hasNextPage":true,"endCursor":"C1"}}}}`
	page2 := `{"data":{"projects":{"nodes":[{"id":"p2","name":"Mobile"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		if after, ok := req.Variables["after"].(string); ok && after == "C1" {
			w.Write([]byte(page2))
			return
		}
		w.Write([]byte(page1))
	}))
	defer srv.Close()

	c := New(srv.URL, "lin_test", srv.Client())
	names, err := c.ProjectNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if names["p1"] != "Core" || names["p2"] != "Mobile" {
		t.Fatalf("bad project names across pages: %+v", names)
	}
}

// TestPaginationAggregatesAcrossPages is the table-driven multi-page check
// CHA-2400 asks for. Issues and ProjectNames each had their own pagination
// test; Comments had none, so a regression there — the query is capped at
// first: 100 — would have silently truncated ingestion on any workspace with
// more than 100 comments in the window, with no test to catch it.
//
// Each case drives three pages so an off-by-one that stops after the second
// still fails, and asserts the cursor chain, not just the total count: a
// client that refetched page 1 twice would otherwise produce a plausible
// aggregate from the wrong data.
func TestPaginationAggregatesAcrossPages(t *testing.T) {
	cases := []struct {
		name      string
		field     string
		nodes     [3]string
		wantCount int
		fetch     func(*Client) (int, error)
	}{
		{
			name:  "issues",
			field: "issues",
			nodes: [3]string{
				`{"id":"i1","identifier":"ENG-1"},{"id":"i2","identifier":"ENG-2"}`,
				`{"id":"i3","identifier":"ENG-3"}`,
				`{"id":"i4","identifier":"ENG-4"},{"id":"i5","identifier":"ENG-5"}`,
			},
			wantCount: 5,
			fetch: func(c *Client) (int, error) {
				got, err := c.Issues(context.Background(), time.Now().Add(-time.Hour))
				return len(got), err
			},
		},
		{
			name:  "comments",
			field: "comments",
			nodes: [3]string{
				`{"id":"c1","body":"one","issue":{"id":"i1"}},{"id":"c2","body":"two","issue":{"id":"i1"}}`,
				`{"id":"c3","body":"three","issue":{"id":"i2"}}`,
				`{"id":"c4","body":"four","issue":{"id":"i3"}}`,
			},
			wantCount: 4,
			fetch: func(c *Client) (int, error) {
				got, err := c.Comments(context.Background(), time.Now().Add(-time.Hour))
				return len(got), err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cursors []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req struct {
					Variables map[string]any `json:"variables"`
				}
				json.Unmarshal(body, &req)
				after, _ := req.Variables["after"].(string)
				cursors = append(cursors, after)

				w.Header().Set("Content-Type", "application/json")
				switch after {
				case "":
					fmt.Fprintf(w, `{"data":{"%s":{"nodes":[%s],"pageInfo":{"hasNextPage":true,"endCursor":"C1"}}}}`, tc.field, tc.nodes[0])
				case "C1":
					fmt.Fprintf(w, `{"data":{"%s":{"nodes":[%s],"pageInfo":{"hasNextPage":true,"endCursor":"C2"}}}}`, tc.field, tc.nodes[1])
				default:
					fmt.Fprintf(w, `{"data":{"%s":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`, tc.field, tc.nodes[2])
				}
			}))
			defer srv.Close()

			got, err := tc.fetch(New(srv.URL, "lin_test", srv.Client()))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.wantCount {
				t.Fatalf("want %d records aggregated across 3 pages, got %d", tc.wantCount, got)
			}
			want := []string{"", "C1", "C2"}
			if len(cursors) != len(want) {
				t.Fatalf("want %d requests (one per page), got %d: %q", len(want), len(cursors), cursors)
			}
			for i := range want {
				if cursors[i] != want[i] {
					t.Fatalf("request %d sent after=%q, want %q (cursor chain broken)", i+1, cursors[i], want[i])
				}
			}
		})
	}
}
