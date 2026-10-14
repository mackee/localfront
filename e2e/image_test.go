//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dc "github.com/ory/dockertest/v3/docker"
)

type fakeImageClient struct {
	images     map[string]*dc.Image
	inspectErr map[string]error
	pullErr    error
	pulls      []dc.PullImageOptions
	tags       []dc.TagImageOptions
}

func (f *fakeImageClient) InspectImage(ref string) (*dc.Image, error) {
	if err := f.inspectErr[ref]; err != nil {
		return nil, err
	}
	if img := f.images[ref]; img != nil {
		return img, nil
	}
	return nil, dc.ErrNoSuchImage
}

func (f *fakeImageClient) PullImage(opts dc.PullImageOptions, _ dc.AuthConfiguration) error {
	f.pulls = append(f.pulls, opts)
	if f.pullErr != nil {
		return f.pullErr
	}
	ref := opts.Repository
	if opts.Tag != "" {
		ref += ":" + opts.Tag
	}
	f.images[ref] = &dc.Image{ID: "pulled-image"}
	return nil
}

func (f *fakeImageClient) TagImage(id string, opts dc.TagImageOptions) error {
	f.tags = append(f.tags, opts)
	f.images[opts.Repo+":"+opts.Tag] = &dc.Image{ID: id}
	return nil
}

func TestParseS3Image(t *testing.T) {
	const digest = "sha256:1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c"
	for _, tc := range []struct {
		input, ref, repo, tag, digestHex string
	}{
		{"rustfs/rustfs:1.0.1", "rustfs/rustfs:1.0.1", "rustfs/rustfs", "1.0.1", ""},
		{"rustfs/rustfs", "rustfs/rustfs:latest", "rustfs/rustfs", "latest", ""},
		{"registry.example:5000/rustfs:1.0.1", "registry.example:5000/rustfs:1.0.1", "registry.example:5000/rustfs", "1.0.1", ""},
		{"registry.example:5000/rustfs", "registry.example:5000/rustfs:latest", "registry.example:5000/rustfs", "latest", ""},
		{"rustfs/rustfs@" + digest, "rustfs/rustfs@" + digest, "rustfs/rustfs", "", strings.TrimPrefix(digest, "sha256:")},
		{"rustfs/rustfs:1.0.1@" + digest, "rustfs/rustfs@" + digest, "rustfs/rustfs", "", strings.TrimPrefix(digest, "sha256:")},
		{"registry.example:5000/rustfs:1.0.1@" + digest, "registry.example:5000/rustfs@" + digest, "registry.example:5000/rustfs", "", strings.TrimPrefix(digest, "sha256:")},
	} {
		t.Run(tc.input, func(t *testing.T) {
			spec, err := parseS3Image(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if spec.ref != tc.ref || spec.repository != tc.repo || spec.tag != tc.tag || spec.digest != tc.digestHex {
				t.Fatalf("parseS3Image(%q) = %+v", tc.input, spec)
			}
		})
	}
	for _, ref := range []string{"", "rustfs/rustfs@sha256:bad", "rustfs/rustfs@md5:abcd"} {
		if _, err := parseS3Image(ref); err == nil {
			t.Errorf("parseS3Image(%q) unexpectedly succeeded", ref)
		}
	}
}

func TestPrepareS3Image(t *testing.T) {
	const digestRef = "rustfs/rustfs@sha256:1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c"
	localTag := "sha256-1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c"
	t.Run("cached digest does not pull", func(t *testing.T) {
		f := &fakeImageClient{images: map[string]*dc.Image{digestRef: {ID: "cached"}}}
		f.pullErr = errors.New("offline")
		repo, tag, err := prepareS3Image(f, "rustfs/rustfs:1.0.1@"+strings.SplitN(digestRef, "@", 2)[1])
		if err != nil || repo != "localfront-e2e-s3" || tag != localTag || len(f.pulls) != 0 || len(f.tags) != 1 {
			t.Fatalf("repo=%q tag=%q err=%v pulls=%v tags=%v", repo, tag, err, f.pulls, f.tags)
		}
	})
	t.Run("missing digest pulls once", func(t *testing.T) {
		f := &fakeImageClient{images: map[string]*dc.Image{}}
		_, _, err := prepareS3Image(f, digestRef)
		if err != nil || len(f.pulls) != 1 || f.pulls[0].Repository != digestRef || f.pulls[0].Tag != "" {
			t.Fatalf("err=%v pulls=%v", err, f.pulls)
		}
	})
	t.Run("cached local tag is reused", func(t *testing.T) {
		f := &fakeImageClient{images: map[string]*dc.Image{
			digestRef: {ID: "cached"}, "localfront-e2e-s3:" + localTag: {ID: "cached"},
		}}
		_, _, err := prepareS3Image(f, digestRef)
		if err != nil || len(f.tags) != 0 || len(f.pulls) != 0 {
			t.Fatalf("err=%v pulls=%v tags=%v", err, f.pulls, f.tags)
		}
	})
	t.Run("different local image fails", func(t *testing.T) {
		f := &fakeImageClient{images: map[string]*dc.Image{
			digestRef: {ID: "correct"}, "localfront-e2e-s3:" + localTag: {ID: "different"},
		}}
		if _, _, err := prepareS3Image(f, digestRef); err == nil {
			t.Fatal("mismatched local tag accepted")
		}
	})
	t.Run("tag override pulls on missing image", func(t *testing.T) {
		f := &fakeImageClient{images: map[string]*dc.Image{}}
		repo, tag, err := prepareS3Image(f, "registry.example:5000/store:v1")
		if err != nil || repo != "registry.example:5000/store" || tag != "v1" || len(f.pulls) != 1 || f.pulls[0].Tag != "v1" {
			t.Fatalf("repo=%q tag=%q err=%v pulls=%v", repo, tag, err, f.pulls)
		}
	})
	t.Run("inspect failure does not pull", func(t *testing.T) {
		f := &fakeImageClient{images: map[string]*dc.Image{}, inspectErr: map[string]error{digestRef: errors.New("daemon unavailable")}}
		if _, _, err := prepareS3Image(f, digestRef); err == nil || len(f.pulls) != 0 {
			t.Fatalf("err=%v pulls=%v", err, f.pulls)
		}
	})
	t.Run("uncached pull failure is fatal", func(t *testing.T) {
		f := &fakeImageClient{images: map[string]*dc.Image{}, pullErr: errors.New("registry unavailable")}
		if _, _, err := prepareS3Image(f, digestRef); err == nil || len(f.pulls) != 1 {
			t.Fatalf("err=%v pulls=%v", err, f.pulls)
		}
	})
}

func TestDefaultS3ImageReferencesStayAligned(t *testing.T) {
	for _, name := range []string{"spa-hosting", "static-and-api", "wildcard-subdomain-s3"} {
		body, err := os.ReadFile(filepath.Join("..", "examples", name, "compose.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "${LOCALFRONT_E2E_S3_IMAGE:-"+defaultS3Image+"}") {
			t.Errorf("%s default RustFS image differs from e2e", name)
		}
	}
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(readme), defaultS3Image); count != 2 {
		t.Errorf("README contains default RustFS image %d times, want 2", count)
	}
}
