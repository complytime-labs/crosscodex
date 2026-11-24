//go:build integration_ingestion

// Package ingestion_test drives the deploy stack with document ingestion end
// to end: deploy/local_up.sh brings up the production compose stack offline
// with deploy/compose.ingestion.yaml (shared object volume) or with
// compose.rustfs.yaml as well (one RustFS bucket), a real PDF is imported
// through the gateway over mTLS, and the stored catalog is read back. No LLM
// is involved: the heading tier structures the converted PDF. Run it with
// `task test:integration:ingestion`.
package ingestion_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	project     = "crosscodex-ingestion-test"
	gatewayPort = "50351"
	// tenantID is tenants.default_tenant in deploy/config.yaml: with tenants
	// disabled, the gateway maps the stack's client certificate to it.
	tenantID = "default"
	// objectsPath is where compose.ingestion.yaml mounts crosscodex-objects
	// in both crosscodexd and ingestion.
	objectsPath = "/var/lib/crosscodex/objects"
	// maxDocumentBytes is ingestion.max_document_bytes for the stack (via
	// test/ingestion/compose.yaml): above the fixture PDF, below the
	// oversize document.
	maxDocumentBytes = 256 * 1024
	fixturePDF       = "testdata/nist-sp800-171r3-excerpt.pdf"
)

var (
	rootDir, outDir, engine string
	composeCmd              string
)

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		Fail(k + " is not set: run task test:integration:ingestion")
	}
	return v
}

var _ = BeforeSuite(func() {
	if os.Getenv("TEST_COMPOSE_CMD") == "" {
		Skip("TEST_COMPOSE_CMD not set: run task test:integration:ingestion")
	}
	composeCmd = os.Getenv("TEST_COMPOSE_CMD")
	engine = mustEnv("TEST_CONTAINER_ENGINE")
	rootDir = mustEnv("TEST_INGESTION_ROOT_DIR")
	outDir = mustEnv("TEST_INGESTION_OUT_DIR")
	// Ingestion validates tenant IDs with the same rule
	// (crosscodex_ingestion/security.py TENANT_ID_PATTERN); a tenant either
	// side refused would fail every conversion before storage is touched.
	Expect(tenant.ValidateTenantID(tenantID)).To(Succeed())
})

// stack is one bring-up of the deploy stack with the given overlays
// (absolute paths, loaded after the offline overlay).
type stack struct {
	env      []string
	certsDir string
	// volumesBefore lists every volume before up; down checks it leaves no
	// other, including anonymous volumes an image's VOLUME creates.
	volumesBefore []string
}

func newStack(overlays []string, extraEnv ...string) *stack {
	certsDir := filepath.Join(outDir, "certs")
	env := append(os.Environ(),
		"ROOT_DIR="+rootDir,
		"COMPOSE_PROJECT_NAME="+project,
		"CROSSCODEX_CERTS_VOLUME="+project+"-certs",
		"CROSSCODEX_CLIENT_CERTS_VOLUME="+project+"-client-certs",
		"CROSSCODEX_ATTESTATION_KEYS_VOLUME="+project+"-attestation-keys",
		"CROSSCODEX_GATEWAY_PORT="+gatewayPort,
		"CROSSCODEX_GATEWAY_CONFIG_DIR="+outDir,
		"CERTS_DIR="+certsDir,
		"OUT_DIR="+outDir,
		"LLM_GATEWAY=litellm",
		"PRAXIS_MODELS_ENDPOINT=false",
		"COMPOSE_CMD="+composeCmd,
		"CONTAINER_ENGINE="+engine,
		"INGESTION_OVERLAYS="+strings.Join(overlays, " "),
		// For deploy:down only: local_up.sh sets its own COMPOSE_OVERLAY.
		"COMPOSE_OVERLAY="+strings.Join(append([]string{filepath.Join(rootDir, "deploy", "compose.offline.yaml")}, overlays...), " "),
		fmt.Sprintf("TEST_INGESTION_MAX_DOCUMENT_BYTES=%d", maxDocumentBytes),
	)
	return &stack{env: append(env, extraEnv...), certsDir: certsDir}
}

// run returns the command's stdout and exit code; stdout and stderr both
// go to GinkgoWriter, which Ginkgo prints for a failed spec. stderr is kept
// out of the result because podman warns there on every call in some setups.
func (s *stack) run(name string, args ...string) (string, int) {
	var stdout, stderr strings.Builder
	cmd := exec.Command(name, args...)
	cmd.Env = s.env
	cmd.Dir = rootDir
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	GinkgoWriter.Printf("$ %s %s\n%s%s\n", name, strings.Join(args, " "), stdout.String(), stderr.String())
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), 0
	case errors.As(err, &exitErr):
		return stdout.String(), exitErr.ExitCode()
	}
	Fail(fmt.Sprintf("run %s: %v", name, err))
	return "", -1
}

// withPodmanEnv runs argv after sourcing test/deploy/podman_env.sh, so every
// engine call sees the runtime view local_up.sh brought the stack up in.
// argv is passed as positional parameters, never interpolated into the script.
func (s *stack) withPodmanEnv(argv ...string) (string, int) {
	script := `. "$ROOT_DIR/test/deploy/podman_env.sh" && exec "$@"`
	return s.run("bash", append([]string{"-c", script, "podman-env"}, argv...)...)
}

func (s *stack) engine(args ...string) string {
	out, code := s.withPodmanEnv(append([]string{engine}, args...)...)
	ExpectWithOffset(1, code).To(BeZero(), out)
	return out
}

// container returns the ID of the project's running container for service.
func (s *stack) container(service string) string {
	out := s.engine("ps", "--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service="+service, "--format", "{{.ID}}")
	id := strings.TrimSpace(out)
	ExpectWithOffset(1, id).NotTo(BeEmpty(), "no running %s container in project %s", service, project)
	return strings.Fields(id)[0]
}

// runHealthchecks runs every running project container's healthcheck each
// period until stop closes. podman schedules healthchecks with systemd
// timers; without systemd (as in a dev container) none run, and compose's
// service_healthy dependencies never resolve. With systemd these are extra
// runs of the same checks. The period is no shorter than any service's
// healthcheck interval, so no retry budget is used up faster than declared.
func (s *stack) runHealthchecks(stop <-chan struct{}, period time.Duration) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		out, code := s.withPodmanEnv(engine, "ps", "--filter", "label=com.docker.compose.project="+project, "--format", "{{.ID}}")
		if code != 0 {
			continue
		}
		for _, id := range strings.Fields(out) {
			// A container without a healthcheck, or one still failing it,
			// exits non-zero; the output is in GinkgoWriter.
			_, _ = s.withPodmanEnv(engine, "healthcheck", "run", id)
		}
	}
}

func (s *stack) up() {
	s.volumesBefore = strings.Fields(s.engine("volume", "ls", "--format", "{{.Name}}"))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer GinkgoRecover()
		s.runHealthchecks(stop, 5*time.Second)
	})
	out, code := s.run("bash", filepath.Join(rootDir, "deploy", "local_up.sh"))
	close(stop)
	wg.Wait()
	Expect(code).To(BeZero(), "deploy/local_up.sh failed (output above):\n%s", out)
}

// down removes the stack's containers, data and key volumes and PKI, and
// checks nothing of the project is left.
func (s *stack) down() {
	out, code := s.withPodmanEnv("task", "deploy:down", "VOLUMES=1")
	Expect(code).To(BeZero(), "deploy:down failed:\n%s", out)
	Expect(os.RemoveAll(s.certsDir)).To(Succeed())

	Expect(strings.TrimSpace(s.engine("ps", "-a", "--filter", "label=com.docker.compose.project="+project, "--format", "{{.Names}}"))).
		To(BeEmpty(), "containers of %s left after teardown", project)
	vols := strings.Fields(s.engine("volume", "ls", "--format", "{{.Name}}"))
	Expect(slices.DeleteFunc(slices.Clone(vols), func(v string) bool { return !strings.HasPrefix(v, project) })).
		To(BeEmpty(), "volumes of %s left after teardown", project)
	if s.volumesBefore != nil {
		Expect(slices.DeleteFunc(vols, func(v string) bool { return slices.Contains(s.volumesBefore, v) })).
			To(BeEmpty(), "volumes created during the run and left after teardown")
	}
}

// storedKeys lists every object in the stack's object storage, as keys
// relative to the storage root (<tenant>/documents/<id>/<file>), from inside
// the ingestion container, which mounts the same volume or bucket as
// crosscodexd. s3 selects the bucket (the credentials stay in the
// container's environment).
func (s *stack) storedKeys(s3 bool) []string {
	script := `import os, sys
root = sys.argv[1]
for d, _, files in os.walk(root):
    for f in files:
        print(os.path.relpath(os.path.join(d, f), root))`
	if s3 {
		script = `import os, boto3
c = boto3.client("s3", endpoint_url=os.environ["S3_ENDPOINT"], region_name=os.environ["S3_REGION"],
    aws_access_key_id=os.environ["S3_ACCESS_KEY"], aws_secret_access_key=os.environ["S3_SECRET_KEY"])
for page in c.get_paginator("list_objects_v2").paginate(Bucket=os.environ["S3_BUCKET"]):
    for o in page.get("Contents", []):
        print(o["Key"])`
	}
	return strings.Fields(s.engine("exec", s.container("ingestion"), "python", "-c", script, objectsPath))
}

// readObject returns the object at key (relative to the storage root).
func (s *stack) readObject(s3 bool, key string) []byte {
	script := `import os, sys
sys.stdout.buffer.write(open(os.path.join(sys.argv[1], sys.argv[2]), "rb").read())`
	if s3 {
		script = `import os, sys, boto3
c = boto3.client("s3", endpoint_url=os.environ["S3_ENDPOINT"], region_name=os.environ["S3_REGION"],
    aws_access_key_id=os.environ["S3_ACCESS_KEY"], aws_secret_access_key=os.environ["S3_SECRET_KEY"])
sys.stdout.buffer.write(c.get_object(Bucket=os.environ["S3_BUCKET"], Key=sys.argv[2])["Body"].read())`
	}
	return []byte(s.engine("exec", s.container("ingestion"), "python", "-c", script, objectsPath, key))
}

func (s *stack) gateway() crosscodexv1connect.GatewayServiceClient {
	cert, err := tls.LoadX509KeyPair(filepath.Join(s.certsDir, "client.pem"), filepath.Join(s.certsDir, "client-key.pem"))
	Expect(err).NotTo(HaveOccurred())
	caPEM, err := os.ReadFile(filepath.Join(s.certsDir, "ca.pem"))
	Expect(err).NotTo(HaveOccurred())
	pool := x509.NewCertPool()
	Expect(pool.AppendCertsFromPEM(caPEM)).To(BeTrue())
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS12,
	}}}
	return crosscodexv1connect.NewGatewayServiceClient(httpClient, "https://localhost:"+gatewayPort)
}

func submit(gw crosscodexv1connect.GatewayServiceClient, content []byte) (*pb.SubmitDocumentResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	resp, err := gw.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
		Source:        &pb.SubmitDocumentRequest_Content{Content: content},
		CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func listCatalogIDs(gw crosscodexv1connect.GatewayServiceClient) []string {
	resp, err := gw.ListCatalogs(context.Background(), connect.NewRequest(&pb.ListCatalogsRequest{
		TenantContext: &pb.TenantContext{TenantId: tenantID},
	}))
	Expect(err).NotTo(HaveOccurred())
	ids := []string{}
	for _, c := range resp.Msg.GetCatalogs() {
		ids = append(ids, c.GetCatalogId())
	}
	return ids
}

// ingestionBehavior is the end-to-end contract for one storage layout.
func ingestionBehavior(name string, overlays func() []string, s3 bool, extraEnv func() []string) {
	// ContinueOnFailure: each spec checks its own part of the stack, so one
	// failure does not hide the others.
	Describe(name, Ordered, ContinueOnFailure, func() {
		var (
			s  *stack
			gw crosscodexv1connect.GatewayServiceClient
		)

		BeforeAll(func() {
			s = newStack(overlays(), extraEnv()...)
			// A run interrupted before its teardown leaves the project behind.
			s.down()
			DeferCleanup(s.down)
			s.up()
			gw = s.gateway()
		})

		It("runs ingestion as the overlay declares", func() {
			id := s.container("ingestion")
			Expect(strings.Fields(s.engine("inspect", "--format",
				"{{.Config.User}} {{.HostConfig.ReadonlyRootfs}} {{.HostConfig.PidsLimit}} {{.HostConfig.Memory}} {{.State.Health.Status}}", id))).
				To(Equal([]string{"65532:65532", "true", "2048", fmt.Sprint(8 << 30), "healthy"}))
			Expect(strings.Fields(s.engine("exec", id, "python", "-c",
				`import os; print(os.getuid(), os.getgid(), os.environ["HOME"])`))).
				To(Equal([]string{"65532", "65532", "/tmp"}))
		})

		if !s3 {
			It("mounts the shared object volume at the same path in crosscodexd and ingestion", func() {
				want := project + "_crosscodex-objects=" + objectsPath
				for _, svc := range []string{"crosscodexd", "ingestion"} {
					Expect(strings.Fields(s.engine("inspect", "--format",
						"{{range .Mounts}}{{.Name}}={{.Destination}} {{end}}", s.container(svc)))).
						To(ContainElement(want), "service %s", svc)
				}
			})
		}

		It("refuses a document over ingestion.max_document_bytes and stores nothing", func() {
			pdf, err := os.ReadFile(fixturePDF)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(pdf)).To(BeNumerically("<", maxDocumentBytes))
			Expect(s.storedKeys(s3)).To(BeEmpty(), "a fresh stack stores no objects")

			// The real PDF padded past the limit: only its size is wrong.
			oversize := make([]byte, maxDocumentBytes+1)
			copy(oversize, pdf)
			_, err = submit(gw, oversize)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeResourceExhausted), "error: %v", err)
			Expect(err.Error()).To(And(
				ContainSubstring(fmt.Sprintf("document is %d bytes, more than ingestion.max_document_bytes (%d)", len(oversize), maxDocumentBytes)),
				ContainSubstring("split the document"),
			))

			Expect(s.storedKeys(s3)).To(BeEmpty(), "the refused document left objects in storage")
			Expect(listCatalogIDs(gw)).To(BeEmpty(), "the refused document created a catalog")
		})

		It("imports a real PDF into a catalog of titled controls with ingestion provenance", func() {
			pdf, err := os.ReadFile(fixturePDF)
			Expect(err).NotTo(HaveOccurred())

			resp, err := submit(gw, pdf)
			Expect(err).NotTo(HaveOccurred())
			docID, catalogID := resp.GetDocumentId(), resp.GetCatalogId()
			Expect(docID).To(HavePrefix("documents/"))
			Expect(catalogID).NotTo(BeEmpty())
			Expect(s.storedKeys(s3)).To(ContainElement(tenantID + "/" + docID))

			var output struct {
				SchemaVersion string `json:"schema_version"`
				Elements      []struct {
					Type    string `json:"type"`
					Content string `json:"content"`
				} `json:"elements"`
			}
			Expect(json.Unmarshal(s.readObject(s3, tenantID+"/"+docID), &output)).To(Succeed())
			Expect(output.SchemaVersion).NotTo(BeEmpty())

			got, err := gw.GetCatalog(context.Background(), connect.NewRequest(&pb.GetCatalogRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID}, CatalogId: catalogID,
			}))
			Expect(err).NotTo(HaveOccurred())
			cat := got.Msg.GetCatalog()
			Expect(cat.GetProvenance().GetLineageMetadata()).To(And(
				HaveKeyWithValue("source_uri", docID),
				HaveKeyWithValue("extractor_name", catalog.IngestionExtractorName),
				HaveKeyWithValue("extractor_version", output.SchemaVersion),
			))
			Expect(listCatalogIDs(gw)).To(ContainElement(catalogID))

			Expect(cat.GetControlCount()).To(BeNumerically(">", 0))

			// The gateway lists controls only by full-text query, so search for
			// each heading of the converted document. Every control the heading
			// tier made is titled with its document heading.
			var headings []string
			for _, e := range output.Elements {
				if e.Type == "heading" {
					headings = append(headings, e.Content)
				}
			}
			Expect(headings).NotTo(BeEmpty())
			found := map[string]*pb.Control{}
			for _, h := range headings {
				resp, err := gw.SearchControls(context.Background(), connect.NewRequest(&pb.SearchControlsRequest{
					TenantContext: &pb.TenantContext{TenantId: tenantID},
					Query:         h,
					CatalogIds:    []string{catalogID},
					Options:       &pb.ListOptions{Pagination: &pb.Pagination{PageSize: 1000}},
				}))
				Expect(err).NotTo(HaveOccurred(), "search %q", h)
				for _, c := range resp.Msg.GetControls() {
					found[c.GetControlId()] = c
				}
			}
			GinkgoWriter.Printf("catalog %s: %d controls, %d found by %d heading searches\n", catalogID, cat.GetControlCount(), len(found), len(headings))
			Expect(found).NotTo(BeEmpty())
			Expect(len(found)).To(Equal(int(cat.GetControlCount())),
				"heading searches found %d of the catalog's %d controls: a control not titled with a document heading is never found", len(found), cat.GetControlCount())
			for id, c := range found {
				Expect(c.GetCatalogId()).To(Equal(catalogID))
				Expect(headings).To(ContainElement(c.GetTitle()), "control %s is not titled with a document heading", id)
			}
		})
	})
}

var _ = Describe("Document ingestion end to end", func() {
	ingestionBehavior("over the shared object volume (INGESTION=1)", func() []string {
		return []string{
			filepath.Join(rootDir, "deploy", "compose.ingestion.yaml"),
			filepath.Join(rootDir, "test", "ingestion", "compose.yaml"),
		}
	}, false, func() []string { return nil })

	ingestionBehavior("over a RustFS bucket (INGESTION=rustfs)", func() []string {
		return []string{
			filepath.Join(rootDir, "deploy", "compose.ingestion.yaml"),
			filepath.Join(rootDir, "deploy", "compose.rustfs.yaml"),
			filepath.Join(rootDir, "test", "ingestion", "compose.yaml"),
		}
	}, true, func() []string {
		return []string{
			"RUSTFS_ACCESS_KEY=" + mustEnv("TEST_INGESTION_RUSTFS_ACCESS_KEY"),
			"RUSTFS_SECRET_KEY=" + mustEnv("TEST_INGESTION_RUSTFS_SECRET_KEY"),
		}
	})
})
