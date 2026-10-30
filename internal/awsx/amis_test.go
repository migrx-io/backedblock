package awsx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublishedAMI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"amis": [
			{"region": "us-east-1", "arch": "arm64", "id": "ami-old", "created": "2026-10-01T00:00:00.000Z"},
			{"region": "us-east-1", "arch": "arm64", "id": "ami-new", "created": "2026-10-15T00:00:00.000Z"},
			{"region": "us-east-1", "arch": "x86_64", "id": "ami-x86", "created": "2026-10-20T00:00:00.000Z"},
			{"region": "eu-west-1", "arch": "arm64", "id": "ami-eu", "created": "2026-10-20T00:00:00.000Z"}
		]}`))
	}))
	defer srv.Close()
	t.Setenv("BBCTL_AMIS_URL", srv.URL)

	for _, c := range []struct{ region, arch, want string }{
		{"us-east-1", "arm64", "ami-new"},
		{"us-east-1", "x86_64", "ami-x86"},
		{"eu-west-1", "arm64", "ami-eu"},
		{"eu-west-1", "x86_64", ""},
		{"sa-east-1", "arm64", ""},
	} {
		im, err := PublishedAMI(context.Background(), c.region, c.arch)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s/%s: got %s, want error", c.region, c.arch, im.ID)
			}
			continue
		}
		if err != nil || im.ID != c.want {
			t.Errorf("%s/%s: got %q, %v; want %s", c.region, c.arch, im.ID, err, c.want)
		}
	}
}

func TestPublishedAMIHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	t.Setenv("BBCTL_AMIS_URL", srv.URL)
	if _, err := PublishedAMI(context.Background(), "us-east-1", "arm64"); err == nil {
		t.Fatal("want error on 404")
	}
}
