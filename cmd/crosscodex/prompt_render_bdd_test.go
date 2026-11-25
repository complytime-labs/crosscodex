package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// renderGateway answers RenderPrompt with resp, or err when set, and
// records the last request; every other RPC is unimplemented.
type renderGateway struct {
	crosscodexv1connect.UnimplementedGatewayServiceHandler
	resp *pb.RenderPromptResponse
	err  error
	got  *pb.RenderPromptRequest
}

func (g *renderGateway) RenderPrompt(_ context.Context, req *connectrpc.Request[pb.RenderPromptRequest]) (*connectrpc.Response[pb.RenderPromptResponse], error) {
	g.got = req.Msg
	if g.err != nil {
		return nil, g.err
	}
	return connectrpc.NewResponse(g.resp), nil
}

var _ = Describe("prompt render", func() {
	run := func(gw *renderGateway, args ...string) (string, error) {
		path, handler := crosscodexv1connect.NewGatewayServiceHandler(gw)
		mux := http.NewServeMux()
		mux.Handle(path, handler)
		srv := httptest.NewServer(mux)
		DeferCleanup(srv.Close)

		root := &cobra.Command{Use: "crosscodex"}
		root.PersistentFlags().Bool("json", false, "")
		root.PersistentFlags().Bool("plain", false, "")
		root.PersistentFlags().Bool("no-color", false, "")
		root.AddCommand(newPromptCmd(&cliState{client: crosscodexv1connect.NewGatewayServiceClient(srv.Client(), srv.URL)}))
		stdout, _, err := execute(root, append([]string{"prompt", "render"}, args...)...)
		return stdout, err
	}

	// Rendered text is untrusted: markup and a multi-line template.
	rendered := &pb.RenderPromptResponse{
		Messages: []*pb.PromptMessage{
			{Role: "system", Content: "You classify.\n<b>bold</b> &amp;"},
			{Role: "user", Content: "<requirement>\nEnforce least privilege.\n</requirement>"},
		},
		PromptName: "relationship", PromptVersion: "1.3.0",
	}

	It("prints each message verbatim between role fences, after the untrusted-text note", func() {
		gw := &renderGateway{resp: rendered}
		out, err := run(gw, "--analyzer", "relationship", "--source", "cat-1/ac-2", "--target", "cat-2/po.1",
			"--source-catalog", "cat-1", "--target-catalog", "cat-2", "--plain")
		Expect(err).NotTo(HaveOccurred())

		Expect(out).To(Equal("Prompt: relationship 1.3.0\n" +
			untrustedPromptNote + "\n\n" +
			"--- system ---\nYou classify.\n<b>bold</b> &amp;\n" +
			"--- user ---\n<requirement>\nEnforce least privilege.\n</requirement>\n" +
			"--- end ---\n"))
		Expect(gw.got.GetAnalyzer()).To(Equal("relationship"))
		Expect(gw.got.GetSourceCatalogId()).To(Equal("cat-1"))
		Expect(gw.got.GetSourceControlId()).To(Equal("cat-1/ac-2"))
		Expect(gw.got.GetTargetCatalogId()).To(Equal("cat-2"))
		Expect(gw.got.GetTargetControlId()).To(Equal("cat-2/po.1"))
		Expect(gw.got.GetVerdict()).To(BeNil())
		Expect(gw.got.GetTenantContext()).To(BeNil())
	})

	It("emits the messages JSON-escaped with --json", func() {
		out, err := run(&renderGateway{resp: rendered}, "--analyzer", "relationship", "--source", "a", "--target", "b",
			"--job", "job-1", "--json")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("<b>"))

		var got struct {
			PromptName    string `json:"prompt_name"`
			PromptVersion string `json:"prompt_version"`
			JobID         string `json:"job_id"`
			Messages      []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got.PromptName).To(Equal("relationship"))
		Expect(got.PromptVersion).To(Equal("1.3.0"))
		Expect(got.JobID).To(Equal("job-1"))
		Expect(got.Messages).To(HaveLen(2))
		Expect(got.Messages[0].Content).To(Equal("You classify.\n<b>bold</b> &amp;"))
	})

	It("sends --verdict and each --justification, commas kept, and --tenant", func() {
		gw := &renderGateway{resp: rendered}
		_, err := run(gw, "--analyzer", "critique", "--source", "a", "--target", "b", "--source-catalog", "c1",
			"--target-catalog", "c2", "--verdict", "EQUIVALENT", "--justification", "One, two.", "--justification", "Three.",
			"--tenant", "acme")
		Expect(err).NotTo(HaveOccurred())
		Expect(gw.got.GetVerdict().GetVerdict()).To(Equal("EQUIVALENT"))
		Expect(gw.got.GetVerdict().GetJustifications()).To(Equal([]string{"One, two.", "Three."}))
		Expect(gw.got.GetTenantContext().GetTenantId()).To(Equal("acme"))
	})

	It("returns the gateway's refusal with its remedy", func() {
		_, err := run(&renderGateway{err: connectrpc.NewError(connectrpc.CodeInvalidArgument,
			errors.New("give either --job <job-id> or --verdict <verdict>"))}, "--analyzer", "critique", "--source", "a", "--target", "b")
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("--job <job-id>"))
	})

	DescribeTable("requires the analyzer and both controls",
		func(args ...string) {
			gw := &renderGateway{resp: rendered}
			_, err := run(gw, args...)
			Expect(err).To(MatchError(ContainSubstring("required flag")))
			Expect(gw.got).To(BeNil())
		},
		Entry("without --analyzer", "--source", "a", "--target", "b"),
		Entry("without --source", "--analyzer", "relationship", "--target", "b"),
		Entry("without --target", "--analyzer", "relationship", "--source", "a"),
	)

	It("needs a connection, unlike prompt list and show, so it renders through the daemon or embedded mode", func() {
		Expect(needsConnection("crosscodex prompt render")).To(BeTrue())
		Expect(needsConnection("crosscodex prompt show")).To(BeFalse())
	})
})
