package awsx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// AMIsURL lists the node images published on the docs site, the same rows as
// the table at https://backedblock.io/docs/node-amis. BBCTL_AMIS_URL overrides
// it, e.g. for a mirror.
const AMIsURL = "https://backedblock.io/amis.json"

type Image struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Arch    string `json:"arch"`
	Created string `json:"created"`
}

type publishedAMI struct {
	Image
	Region string `json:"region"`
}

// PublishedAMI returns the node image the docs site lists for a region and
// architecture.
func PublishedAMI(ctx context.Context, region, arch string) (Image, error) {
	url := AMIsURL
	if u := os.Getenv("BBCTL_AMIS_URL"); u != "" {
		url = u
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Image{}, fmt.Errorf("fetching published AMIs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Image{}, fmt.Errorf("fetching published AMIs: GET %s: %s", url, resp.Status)
	}
	var doc struct {
		AMIs []publishedAMI `json:"amis"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return Image{}, fmt.Errorf("parsing %s: %w", url, err)
	}
	return pickAMI(doc.AMIs, region, arch)
}

func pickAMI(amis []publishedAMI, region, arch string) (Image, error) {
	var best Image
	for _, a := range amis {
		if a.Region == region && a.Arch == arch && a.Created >= best.Created {
			best = a.Image
		}
	}
	if best.ID == "" {
		return Image{}, fmt.Errorf("no %s node AMI published for %s", arch, region)
	}
	return best, nil
}
