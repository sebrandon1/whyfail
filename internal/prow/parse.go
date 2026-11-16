// Package prow parses Prow and GCS identifiers used by the OpenShift CI.
package prow

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	ProwHost     = "prow.ci.openshift.org"
	GCSHost      = "storage.googleapis.com"
	GCSWebHosts  = "gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com"
	PublicBucket = "test-platform-results-public"
)

// Ref identifies a single Prow job run inside a GCS bucket.
type Ref struct {
	Bucket     string // e.g. test-platform-results-public
	ObjectPath string // e.g. pr-logs/pull/org_repo/1234/job/2123...
	Job        string // last-but-one path segment
	RunID      string // last path segment
}

// ParseRef accepts a Prow view URL, a gs:// URL, or a bare bucket/path.
func ParseRef(raw string) (Ref, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Ref{}, fmt.Errorf("empty reference")
	}

	if strings.HasPrefix(raw, "gs://") {
		return refFromBare(strings.TrimPrefix(raw, "gs://"))
	}

	u, err := url.Parse(raw)
	if err != nil {
		return Ref{}, fmt.Errorf("parse %q: %w", raw, err)
	}

	switch {
	case u.Host == ProwHost:
		p := u.Path
		switch {
		case strings.HasPrefix(p, "/view/gs/"):
			return refFromBare(strings.TrimPrefix(p, "/view/gs/"))
		case strings.HasPrefix(p, "/view/gcs/"):
			return refFromBare(strings.TrimPrefix(p, "/view/gcs/"))
		}
		return Ref{}, fmt.Errorf("unsupported Prow path %q (want /view/gs/<bucket>/...)", p)
	case u.Host == GCSHost && (strings.HasPrefix(u.Path, "/storage/v1/") || strings.HasPrefix(u.Path, "/download/storage/v1/")):
		// API-style URL cannot be resolved to a run; reject with guidance.
		return Ref{}, fmt.Errorf("storage API URL is not a run reference; pass a Prow view URL or gs:// URL")
	case u.Host == GCSWebHosts:
		if i := strings.Index(u.Path, "/gcs/"); i >= 0 {
			return refFromBare(u.Path[i+len("/gcs/"):])
		}
	}
	return Ref{}, fmt.Errorf("not a Prow run reference: %q", raw)
}

// stripNestedScheme drops a gs:// that appears inside an object path. It comes
// from shell interpolation such as gs://$BUCKET/$PATH where the path already is
// a gs:// URL, or from pasting a gs:// URL into a Prow view URL's /view/gs/
// slot. The inner reference is the one meant, and no object path can contain a
// scheme of its own, so returning what follows it is unambiguous.
func stripNestedScheme(s string) string {
	i := strings.Index(s, "gs://")
	if i < 0 {
		return s
	}
	return s[i+len("gs://"):]
}

func refFromBare(s string) (Ref, error) {
	s = stripNestedScheme(strings.Trim(s, "/"))
	bucket, path, ok := strings.Cut(s, "/")
	if !ok || bucket == "" || path == "" {
		return Ref{}, fmt.Errorf("want <bucket>/<path>, got %q", s)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return Ref{}, fmt.Errorf("want at least <job>/<run-id> in %q", path)
	}
	return Ref{
		Bucket:     bucket,
		ObjectPath: strings.Join(parts, "/"),
		Job:        parts[len(parts)-2],
		RunID:      parts[len(parts)-1],
	}, nil
}

// ObjectURL returns the public download URL for an object in the ref's bucket.
func ObjectURL(bucket, object string) string {
	u := url.URL{Scheme: "https", Host: GCSHost, Path: "/" + bucket + "/" + object}
	return u.String()
}

// ViewURL returns the Prow view URL for a run.
func ViewURL(ref Ref) string {
	return "https://" + ProwHost + "/view/gs/" + ref.Bucket + "/" + ref.ObjectPath
}

// ParseArtifactURL extracts bucket and object from a GCS artifact URL.
func ParseArtifactURL(raw string) (bucket, object string, err error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "gs://") {
		s := stripNestedScheme(strings.TrimPrefix(raw, "gs://"))
		b, o, ok := strings.Cut(s, "/")
		if !ok {
			return "", "", fmt.Errorf("want gs://<bucket>/<object>, got %q", raw)
		}
		return b, o, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	switch u.Host {
	case GCSHost:
		p := strings.TrimPrefix(u.Path, "/")
		b, o, ok := strings.Cut(p, "/")
		if !ok {
			return "", "", fmt.Errorf("not an object URL: %q", raw)
		}
		return b, o, nil
	case GCSWebHosts:
		if i := strings.Index(u.Path, "/gcs/"); i >= 0 {
			b, o, ok := strings.Cut(u.Path[i+len("/gcs/"):], "/")
			if ok {
				return b, o, nil
			}
		}
	}
	return "", "", fmt.Errorf("not a GCS artifact URL: %q", raw)
}

// IsRunURL reports whether raw looks like a Prow run reference.
func IsRunURL(raw string) bool {
	_, err := ParseRef(raw)
	return err == nil
}

// Repo returns the "org/repo" a presubmit object path belongs to. Periodic
// jobs live under logs/<job>/ and carry no repository, so they return "".
// The org and repo share one path segment, joined by an underscore
// (pr-logs/pull/rh-ecosystem-edge_recert/2364/<job>/<run>); a repository name
// containing an underscore is split at the first one.
func Repo(objectPath string) string {
	parts := strings.Split(strings.Trim(objectPath, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] != "pull" {
			continue
		}
		if org, repo, ok := strings.Cut(parts[i+1], "_"); ok && org != "" && repo != "" {
			return org + "/" + repo
		}
		return ""
	}
	return ""
}
