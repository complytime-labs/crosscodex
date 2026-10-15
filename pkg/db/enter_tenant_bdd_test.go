//go:build !integration

package db_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// directTenantScope matches the statements that scope a transaction to a
// tenant without enter_tenant: set_config('app.current_tenant', ...) with any
// case, whitespace or quote style, and SET [LOCAL|SESSION] app.current_tenant.
// current_setting reads do not match. Only Go files are scanned, not SQL
// migrations, which define enter_tenant itself.
var directTenantScope = regexp.MustCompile(
	`(?i)\bset_config\s*\(\s*['"]app\.current_tenant['"]|\bset\s+(local\s+|session\s+)?app\.current_tenant\b`)

// Every app_user and graph_user transaction must scope through
// enter_tenant, which refuses deleted tenants. Retention keeps set_config:
// a deleted tenant's job data still follows retention. NewRetentionTenantPool's
// statement in tenant_pool.go is the one other permitted occurrence.
var _ = Describe("tenant scoping", func() {
	DescribeTable("directTenantScope",
		func(text string, wantMatch bool) {
			Expect(directTenantScope.MatchString(text)).To(Equal(wantMatch))
		},
		Entry("set_config call", "SELECT set_config('app.current_tenant', $1, true)", true),
		Entry("space after the paren", "set_config( 'app.current_tenant', $1, true)", true),
		Entry("upper case", "SELECT SET_CONFIG('app.current_tenant', $1, true)", true),
		Entry("double quotes", `set_config("app.current_tenant", $1, true)`, true),
		Entry("newline before the name", "set_config(\n\t'app.current_tenant'", true),
		Entry("SET LOCAL", "SET LOCAL app.current_tenant = 'x'", true),
		Entry("SET SESSION", "set session app.current_tenant = 'x'", true),
		Entry("bare SET", "SET app.current_tenant TO 'x'", true),
		Entry("enter_tenant", "SELECT public.enter_tenant($1)", false),
		Entry("current_setting read", "current_setting('app.current_tenant', true)", false),
		Entry("set_config of another setting", "set_config('app.current_user', $1, true)", false),
	)

	It("never sets app.current_tenant directly outside retention", func() {
		root := filepath.Join("..", "..")
		allowed := map[string]int{
			filepath.Join("pkg", "retention", "purge.go"):   1,
			filepath.Join("pkg", "retention", "archive.go"): 1,
			filepath.Join("pkg", "db", "tenant_pool.go"):    1,
		}
		skipDirs := map[string]bool{".worktrees": true, "vendor": true, ".git": true, "node_modules": true}
		var offenders []string
		Expect(filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if len(directTenantScope.FindAll(b, -1)) > allowed[rel] {
				offenders = append(offenders, rel)
			}
			return nil
		})).To(Succeed())
		Expect(offenders).To(BeEmpty(), "use SELECT public.enter_tenant($1) instead")
	})
})
