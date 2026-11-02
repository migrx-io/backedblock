package awsx

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 is a versioned bucket: key -> version IDs, plus delete markers.
type fakeS3 struct {
	mu       sync.Mutex
	bucket   string
	versions map[string][]string
	markers  map[string][]string
	deleted  bool // the bucket
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/"+f.bucket) {
		w.WriteHeader(404)
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && q.Has("versions"):
		prefix, max := q.Get("prefix"), 1000
		if q.Get("max-keys") == "1" {
			max = 1
		}
		var b strings.Builder
		b.WriteString(`<ListVersionsResult><Name>` + f.bucket + `</Name><IsTruncated>false</IsTruncated>`)
		n := 0
		seen := map[string]bool{}
		var keys []string
		for _, m := range []map[string][]string{f.versions, f.markers} {
			for k := range m {
				if !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			for _, v := range f.versions[k] {
				if n < max {
					fmt.Fprintf(&b, `<Version><Key>%s</Key><VersionId>%s</VersionId></Version>`, k, v)
					n++
				}
			}
			for _, v := range f.markers[k] {
				if n < max {
					fmt.Fprintf(&b, `<DeleteMarker><Key>%s</Key><VersionId>%s</VersionId></DeleteMarker>`, k, v)
					n++
				}
			}
		}
		b.WriteString(`</ListVersionsResult>`)
		w.Write([]byte(b.String()))
	case r.Method == "POST" && q.Has("delete"):
		var in struct {
			Objects []struct {
				Key       string `xml:"Key"`
				VersionID string `xml:"VersionId"`
			} `xml:"Object"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = xml.Unmarshal(body, &in)
		for _, o := range in.Objects {
			f.versions[o.Key] = without(f.versions[o.Key], o.VersionID)
			f.markers[o.Key] = without(f.markers[o.Key], o.VersionID)
			if len(f.versions[o.Key]) == 0 && len(f.markers[o.Key]) == 0 {
				delete(f.versions, o.Key)
				delete(f.markers, o.Key)
			}
		}
		w.Write([]byte(`<DeleteResult></DeleteResult>`))
	case r.Method == "DELETE" && r.URL.Path == "/"+f.bucket:
		if len(f.versions) > 0 || len(f.markers) > 0 {
			w.WriteHeader(409)
			w.Write([]byte(`<Error><Code>BucketNotEmpty</Code></Error>`))
			return
		}
		f.deleted = true
		w.WriteHeader(204)
	default:
		w.WriteHeader(400)
	}
}

func without(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func fakeClient(t *testing.T, f *fakeS3) *Client {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Client{S3: s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  aws.AnonymousCredentials{},
	})}
}

func TestPurgeState(t *testing.T) {
	// dev's state, with old versions, a lock and a delete marker.
	f := &fakeS3{bucket: "b", versions: map[string][]string{
		"dev/network.tfstate":        {"n1", "n2"},
		"dev/pools/pool1.tfstate":    {"p1"},
		"dev/network.tfstate.tflock": {"l1"},
	}, markers: map[string][]string{"dev/mgmt.tfstate": {"m1"}}}
	c := fakeClient(t, f)
	n, deleted, err := c.PurgeState(context.Background(), "b", "dev/")
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || !deleted || !f.deleted {
		t.Errorf("deleted %d versions, bucket %v/%v; want 5, true", n, deleted, f.deleted)
	}
}

func TestPurgeStateKeepsSharedBucket(t *testing.T) {
	// Another deployment, devx, shares the bucket: its prefix starts with
	// "dev" too, so the purge must match "dev/" exactly.
	f := &fakeS3{bucket: "b", versions: map[string][]string{
		"dev/network.tfstate":  {"n1"},
		"devx/network.tfstate": {"x1"},
	}, markers: map[string][]string{}}
	c := fakeClient(t, f)
	n, deleted, err := c.PurgeState(context.Background(), "b", "dev/")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || deleted || f.deleted {
		t.Errorf("deleted %d versions, bucket %v; want 1, kept", n, deleted)
	}
	if _, ok := f.versions["devx/network.tfstate"]; !ok {
		t.Error("devx's state was deleted")
	}
}

func TestPurgeStateMissingBucket(t *testing.T) {
	c := fakeClient(t, &fakeS3{bucket: "other"})
	n, deleted, err := c.PurgeState(context.Background(), "b", "dev/")
	if err != nil || n != 0 || deleted {
		t.Errorf("missing bucket: %d %v %v; want nothing, no error", n, deleted, err)
	}
}
