//go:build live

package run

import (
	"context"
	"testing"

	"github.com/sebrandon1/whyfail/internal/gcs"
	"github.com/sebrandon1/whyfail/internal/prow"
)

// The live suite proves the same invariants as the recorded fixtures, but
// against the real bucket. If it disagrees with recert_test.go, Prow's artifact
// layout has drifted and the corpus needs `make fixtures`.

const recertBase = "https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/rh-ecosystem-edge_recert/2364/"

var liveRuns = map[string]string{
	"ipc":       recertBase + "pull-ci-rh-ecosystem-edge-recert-main-ipc-e2e-flow/2120987688944275456",
	"serial":    recertBase + "pull-ci-rh-ecosystem-edge-recert-main-e2e-aws-ovn-single-node-recert-serial/2120987680551473152",
	"baremetal": recertBase + "pull-ci-rh-ecosystem-edge-recert-main-baremetalds-sno-recert-cluster-rename/2120987667142283264",
	// A different org, a Sippy-tracked job, and a bare artifacts/junit.xml
	// instead of extension_test_result JSON.
	"cert-manager": "https://prow.ci.openshift.org/view/gs/test-platform-results-public/pr-logs/pull/openshift_cert-manager-operator/533/pull-ci-openshift-cert-manager-operator-master-e2e-operator/2121292213823475712",
}

func TestLiveRecertDigests(t *testing.T) {
	asserts := map[string]func(*testing.T, *Digest){
		"ipc":          assertIPC,
		"serial":       assertSerial,
		"baremetal":    assertBaremetal,
		"cert-manager": assertCertManager,
	}
	for name, raw := range liveRuns {
		t.Run(name, func(t *testing.T) {
			ref, err := prow.ParseRef(raw)
			if err != nil {
				t.Fatalf("parse %s: %v", raw, err)
			}
			c := gcs.New(t.TempDir())
			d, err := Build(context.Background(), c, ref, Options{History: false})
			if err != nil {
				t.Fatalf("build %s: %v (network failure, not necessarily drift)", raw, err)
			}
			if d.Artifacts == nil || d.Artifacts.Count == 0 {
				t.Fatalf("%s is no longer retained in the bucket (empty listing); "+
					"refresh the fixtures or pick a newer run", raw)
			}
			asserts[name](t, d)
		})
	}
}
