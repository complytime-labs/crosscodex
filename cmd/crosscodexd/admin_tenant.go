package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Seams: specs replace these to exercise parsing, output and exit codes
// without a database.
var (
	runTenantCreateFn    = daemonTenantCreate
	runTenantListFn      = daemonTenantList
	runTenantInspectFn   = daemonTenantInspect
	runTenantSetStatusFn = daemonTenantSetStatus
	runTenantImportFn    = daemonTenantImport
)

// tenantAdminDSN loads config for cmd and returns database.tenant_admin_dsn,
// printing why when it cannot. These commands never use the app or owner
// DSN, and never run migrations.
func tenantAdminDSN(ctx context.Context, cmd string) (string, bool) {
	cfg, ok := loadAdminConfig(ctx, cmd)
	if !ok {
		return "", false
	}
	if cfg.Database.TenantAdminDSN == "" {
		fmt.Fprintf(os.Stderr, "%s: database.tenant_admin_dsn is not set. Set CROSSCODEX_DATABASE_TENANT_ADMIN_DSN to a DSN for the tenant_admin role (created by migration 007_tenant_admin).\n", cmd)
		return "", false
	}
	return cfg.Database.TenantAdminDSN, true
}

// tenantFlag validates a required --tenant value, printing why it is unusable.
func tenantFlag(cmd, id string) bool {
	if id == "" {
		fmt.Fprintf(os.Stderr, "%s: --tenant is required\n", cmd)
		return false
	}
	if err := tenant.ValidateTenantID(id); err != nil {
		fmt.Fprintf(os.Stderr, "%s: --tenant: %v\n", cmd, err)
		return false
	}
	return true
}

func tenantCreateCmd(args []string) int {
	const cmd = "admin tenant create"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	id := fs.String("tenant", "", "ID of the tenant to create, or to rename if it exists (required)")
	name := fs.String("display-name", "", "human-readable tenant name (required)")
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	if !tenantFlag(cmd, *id) {
		return 2
	}
	if err := dbpkg.ValidateTenantSpec(dbpkg.TenantSpec{ID: *id, DisplayName: *name}); err != nil {
		fmt.Fprintf(os.Stderr, "%s: --display-name: %v\n", cmd, err)
		return 2
	}
	ctx := context.Background()
	dsn, ok := tenantAdminDSN(ctx, cmd)
	if !ok {
		return 1
	}
	created, err := runTenantCreateFn(ctx, dsn, *id, *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	if created {
		fmt.Fprintln(os.Stdout, "created")
	} else {
		fmt.Fprintln(os.Stdout, "updated")
	}
	return 0
}

func tenantListCmd(args []string) int {
	const cmd = "admin tenant list"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	ctx := context.Background()
	dsn, ok := tenantAdminDSN(ctx, cmd)
	if !ok {
		return 1
	}
	records, err := runTenantListFn(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	writeTenantList(os.Stdout, records)
	return 0
}

func tenantInspectCmd(args []string) int {
	const cmd = "admin tenant inspect"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	id := fs.String("tenant", "", "ID of the tenant to show (required)")
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	if !tenantFlag(cmd, *id) {
		return 2
	}
	ctx := context.Background()
	dsn, ok := tenantAdminDSN(ctx, cmd)
	if !ok {
		return 1
	}
	record, err := runTenantInspectFn(ctx, dsn, *id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	writeTenantRecord(os.Stdout, record)
	return 0
}

// tenantSetStatusCmd runs `suspend` (status suspended) or `resume`
// (status active).
func tenantSetStatusCmd(args []string, sub, status string) int {
	cmd := "admin tenant " + sub
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	id := fs.String("tenant", "", "ID of the tenant to "+sub+" (required)")
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	if !tenantFlag(cmd, *id) {
		return 2
	}
	ctx := context.Background()
	dsn, ok := tenantAdminDSN(ctx, cmd)
	if !ok {
		return 1
	}
	previous, err := runTenantSetStatusFn(ctx, dsn, *id, status)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	switch {
	case previous == status && status == dbpkg.TenantStatusSuspended:
		fmt.Fprintln(os.Stdout, "already suspended")
	case previous == status:
		fmt.Fprintln(os.Stdout, "already active")
	case status == dbpkg.TenantStatusSuspended:
		fmt.Fprintln(os.Stdout, "suspended")
	default:
		fmt.Fprintln(os.Stdout, "resumed")
	}
	return 0
}

func tenantImportCmd(args []string) int {
	const cmd = "admin tenant import"
	fs := flag.NewFlagSet("crosscodexd "+cmd, flag.ContinueOnError)
	path := fs.String("file", "", "YAML file listing the tenants to create or rename (required)")
	if code, ok := parseNoArgs(fs, cmd, args); !ok {
		return code
	}
	if *path == "" {
		fmt.Fprintf(os.Stderr, "%s: --file is required\n", cmd)
		return 2
	}
	f, err := os.Open(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	specs, err := parseTenantImport(f)
	err = errors.Join(err, f.Close())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s: %v\n", cmd, *path, err)
		return 1
	}
	ctx := context.Background()
	dsn, ok := tenantAdminDSN(ctx, cmd)
	if !ok {
		return 1
	}
	created, err := runTenantImportFn(ctx, dsn, specs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	writeTenantImport(os.Stdout, specs, created)
	return 0
}

// tenantImportFile is the `admin tenant import` file format.
type tenantImportFile struct {
	Tenants []tenantImportEntry `yaml:"tenants"`
}

type tenantImportEntry struct {
	TenantID    string `yaml:"tenant_id"`
	DisplayName string `yaml:"display_name"`
}

// parseTenantImport decodes an import file and validates it as
// TenantAdmin.Import does, so a bad file fails before any connection.
// Unknown fields and a second YAML document are rejected rather than
// silently ignored.
func parseTenantImport(r io.Reader) ([]dbpkg.TenantSpec, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var file tenantImportFile
	if err := dec.Decode(&file); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file is empty; list tenants under a top-level 'tenants:' key")
		}
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, errors.New("file holds more than one YAML document; put every tenant under one 'tenants:' list")
	}
	specs := make([]dbpkg.TenantSpec, len(file.Tenants))
	for i, t := range file.Tenants {
		specs[i] = dbpkg.TenantSpec{ID: t.TenantID, DisplayName: t.DisplayName}
	}
	if err := dbpkg.ValidateTenantSpecs(specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// withTenantAdmin opens a TenantAdmin on dsn, runs fn, and closes it.
func withTenantAdmin(dsn string, fn func(*dbpkg.TenantAdmin) error) (err error) {
	admin, err := dbpkg.OpenTenantAdmin(dsn)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, admin.Close()) }()
	return fn(admin)
}

func daemonTenantCreate(ctx context.Context, dsn, id, name string) (created bool, err error) {
	err = withTenantAdmin(dsn, func(a *dbpkg.TenantAdmin) (err error) {
		created, err = a.Provision(ctx, id, name)
		return err
	})
	return created, err
}

func daemonTenantList(ctx context.Context, dsn string) (records []dbpkg.TenantRecord, err error) {
	err = withTenantAdmin(dsn, func(a *dbpkg.TenantAdmin) (err error) {
		records, err = a.List(ctx)
		return err
	})
	return records, err
}

func daemonTenantInspect(ctx context.Context, dsn, id string) (record dbpkg.TenantRecord, err error) {
	err = withTenantAdmin(dsn, func(a *dbpkg.TenantAdmin) (err error) {
		record, err = a.Get(ctx, id)
		return err
	})
	return record, err
}

func daemonTenantSetStatus(ctx context.Context, dsn, id, status string) (previous string, err error) {
	err = withTenantAdmin(dsn, func(a *dbpkg.TenantAdmin) (err error) {
		previous, err = a.SetStatus(ctx, id, status)
		return err
	})
	return previous, err
}

func daemonTenantImport(ctx context.Context, dsn string, specs []dbpkg.TenantSpec) (created []bool, err error) {
	err = withTenantAdmin(dsn, func(a *dbpkg.TenantAdmin) (err error) {
		created, err = a.Import(ctx, specs)
		return err
	})
	return created, err
}

// displayCell quotes s when it holds a control character, so values
// written outside provision_tenant cannot break the table or the terminal.
func displayCell(s string) string {
	if strings.ContainsFunc(s, unicode.IsControl) {
		return strconv.Quote(s)
	}
	return s
}

func writeTenantList(w io.Writer, records []dbpkg.TenantRecord) {
	if len(records) == 0 {
		fmt.Fprintln(w, "no tenants")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TENANT\tDISPLAY NAME\tSTATUS\tCREATED")
	for _, r := range records {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", displayCell(r.ID), displayCell(r.DisplayName), displayCell(r.Status), r.CreatedAt.UTC().Format(time.RFC3339))
	}
	flushTable(tw, w)
}

func writeTenantRecord(w io.Writer, r dbpkg.TenantRecord) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "tenant_id\t%s\n", displayCell(r.ID))
	fmt.Fprintf(tw, "display_name\t%s\n", displayCell(r.DisplayName))
	fmt.Fprintf(tw, "status\t%s\n", displayCell(r.Status))
	fmt.Fprintf(tw, "created_at\t%s\n", r.CreatedAt.UTC().Format(time.RFC3339))
	flushTable(tw, w)
}

func writeTenantImport(w io.Writer, specs []dbpkg.TenantSpec, created []bool) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TENANT\tRESULT")
	for i, s := range specs {
		result := "updated"
		if created[i] {
			result = "created"
		}
		fmt.Fprintf(tw, "%s\t%s\n", displayCell(s.ID), result)
	}
	flushTable(tw, w)
}
