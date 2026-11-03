package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// profileCheckNote is the help text both editing commands share about what
// their load check covers.
const profileCheckNote = `With --profile, the check also loads that profile, and only that profile:
a change can still break a profile it does not load.`

var (
	// errNotAValue is returned when config set names a section or list,
	// which a single value cannot replace.
	errNotAValue = errors.New("path names a section, not a value")

	// errNotASection is returned when a key before the last one in the
	// path is an alias or a value, or when the edit would change a node an
	// alias refers to: config set and unset never edit through an alias or
	// an anchor in use (the change would reach every place sharing it, or
	// silently drop what the alias inherited) and never turn a value into a
	// section.
	errNotASection = errors.New("path runs through an alias or a value")

	// errKeyNotSet is returned when config unset names a key the user
	// config does not set itself.
	errKeyNotSet = errors.New("key is not set in the user config")
)

func newConfigSetCmd(_ *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Long: `Set a configuration value in the user config file.

Key format uses dot-notation (e.g., cli.output, cli.endpoint).
The value is written to $XDG_CONFIG_HOME/crosscodex/config.yaml, and only
if the configuration still loads with it: a misspelled key or an invalid
value is refused and the file is left unchanged. While the user config
itself fails to load, a change is written only if it makes it load.
` + profileCheckNote,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("requires both key and value")
			}

			key := args[0]
			value := args[1]
			profile, _ := cmd.Flags().GetString("profile")

			remaining, err := editUserConfig(key, profile, func(root *yaml.Node, keys []string, _ string) error {
				return setYAMLPath(root, keys, value)
			})
			if err != nil {
				return fmt.Errorf("set config: %w", err)
			}
			warnStillBroken(cmd, remaining)

			return emit(cmd, func(w io.Writer, color bool) {
				fmt.Fprintf(w, "Set %s = %s\n", key, value)
			}, map[string]string{"key": key, "value": value})
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(2), argGuide{
		noun:  "a key and value",
		find:  "config set",
		usage: "crosscodex config set <key> <value>",
		examples: []string{
			"crosscodex config set cli.output json",
			"crosscodex config set cli.endpoint localhost:50051",
		},
	})

	return cmd
}

func newConfigUnsetCmd(_ *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unset <key>",
		Short: "Remove a key from the user config",
		Long: `Remove a key from the user config file, so the value from a lower layer
(or the compiled default) applies again. It also removes a misspelled or
removed key that stops the configuration loading.

Key format uses dot-notation (e.g., cli.output). The change is written to
$XDG_CONFIG_HOME/crosscodex/config.yaml under the same rules as config set:
only if the configuration still loads (or, while the user config fails to
load, only if the change makes it load), and the file is otherwise left
unchanged.
` + profileCheckNote,
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			profile, _ := cmd.Flags().GetString("profile")

			remaining, err := editUserConfig(key, profile, unsetYAMLPath)
			if err != nil {
				return fmt.Errorf("unset config: %w", err)
			}
			warnStillBroken(cmd, remaining)

			return emit(cmd, func(w io.Writer, color bool) {
				fmt.Fprintf(w, "Unset %s\n", key)
			}, map[string]string{"key": key})
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a configuration key",
		find:  "crosscodex config show",
		usage: "crosscodex config unset <key>",
		examples: []string{
			"crosscodex config unset cli.output",
			"crosscodex config unset pipeline.stage_timout",
		},
	})

	return cmd
}

// editUserConfig applies edit to the user config's root mapping, with key
// split by config.SplitPath, and writes
// the result, but only when checkEdit allows it; remaining is the load error
// that is left when the written edit was a partial repair: a file that fails to load
// stops every crosscodex command. A refused or failed edit leaves the file
// byte-identical. The file is replaced atomically, keeping its mode and
// owner, and a symlinked config is written through to its target.
func editUserConfig(key, profile string, edit func(root *yaml.Node, keys []string, userPath string) error) (remaining, err error) {
	keys, err := config.SplitPath(key)
	if err != nil {
		return nil, err
	}
	userPath := userConfigPath()

	if err := os.MkdirAll(filepath.Dir(userPath), 0o755); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}

	writePath, err := resolveUserConfigLink(userPath)
	if err != nil {
		return nil, err
	}

	var root yaml.Node
	var target os.FileInfo
	data, err := os.ReadFile(writePath)
	switch {
	case err == nil:
		if target, err = os.Stat(writePath); err != nil {
			return nil, fmt.Errorf("stat user config: %w", err)
		}
		if err := yaml.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("parse user config %s: %w. Fix the YAML syntax by hand; config set and unset cannot edit a file they cannot parse", userPath, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// No user config yet: config set creates it.
	default:
		return nil, fmt.Errorf("read user config: %w", err)
	}
	if len(root.Content) == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}

	if err := refuseSharedAnchors(root.Content[0], keys); err != nil {
		return nil, fmt.Errorf("%s was not written: %w", userPath, err)
	}
	if err := edit(root.Content[0], keys, userPath); err != nil {
		return nil, fmt.Errorf("%s was not written: %w", userPath, err)
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}

	remaining, err = checkEdit(userPath, key, profile, out)
	if err != nil {
		return nil, fmt.Errorf("%s was not written: %w", userPath, err)
	}

	return remaining, writeFileAtomic(writePath, out, target)
}

// warnStillBroken tells the operator, after a partial repair was written,
// which problems remain.
func warnStillBroken(cmd *cobra.Command, remaining error) {
	if remaining != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: the change was written, but %s\n", renderError(remaining))
	}
}

// resolveUserConfigLink returns the path to write the user config at
// userPath to: userPath itself, or the target of the symlink it is. A
// dangling symlink is refused rather than replaced by a regular file, which
// would cut the link a dotfile manager maintains.
func resolveUserConfigLink(userPath string) (string, error) {
	info, err := os.Lstat(userPath)
	if errors.Is(err, fs.ErrNotExist) {
		return userPath, nil
	}
	if err != nil {
		return "", fmt.Errorf("stat user config: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return userPath, nil
	}
	target, err := filepath.EvalSymlinks(userPath)
	if errors.Is(err, fs.ErrNotExist) {
		link, linkErr := os.Readlink(userPath)
		if linkErr != nil {
			return "", fmt.Errorf("read user config symlink: %w", linkErr)
		}
		return "", fmt.Errorf("%s is a symlink to %s, which does not exist; create that file (it may be empty) "+
			"or remove the link, then run this again", userPath, link)
	}
	if err != nil {
		return "", fmt.Errorf("resolve user config symlink: %w", err)
	}
	return target, nil
}

// checkEdit decides whether candidate, the edited user config, may be
// written:
//
//   - it may if the configuration loads with it;
//   - otherwise, if the configuration loads now, the edit would break it;
//   - otherwise judgeRepair decides.
//
// remaining, set only when the edit is allowed but the configuration still
// does not load, is the candidate's load error.
func checkEdit(userPath, key, profile string, candidate []byte) (remaining, err error) {
	load := func(extra ...config.Option) error {
		_, err := config.NewLoader().Load(context.Background(), cliLoadOptions(profile, extra...)...)
		return err
	}

	candidateErr := load(config.WithUserConfigContent(candidate))
	if candidateErr == nil {
		return nil, nil
	}
	currentErr := load()
	if currentErr == nil {
		// The value is not quoted back: it may be a credential (database.dsn).
		return nil, fmt.Errorf("with this change to %s the configuration would not load, and every crosscodex command "+
			"would fail. Correct the key or value and run this again (if the value depends on another setting, "+
			"set that one first): %w", key, candidateErr)
	}
	return judgeRepair(userPath, key, currentErr, candidateErr, func() error {
		return load(config.WithUserConfigContent([]byte{}))
	})
}

// judgeRepair decides an edit to the user config at userPath when the
// configuration fails to load both before (currentErr) and after
// (candidateErr) it:
//
//   - if a current problem comes from another layer (its FieldError.Source
//     is another file or an environment variable), config set and unset
//     cannot fix it, so the edit is refused, naming that layer. A problem
//     whose value no layer set (config.SourceDefaults) is not another
//     layer's: the user config can supply it. When some problem has no
//     path, the sources are unknown, so another layer counts as broken if
//     the configuration fails without any user config (loadWithoutUser).
//   - otherwise the edit is written if it is a strict repair: every
//     problem, before and after, is a config.FieldError, and the problems
//     after (counted by setting path) are a strict subset of those before.
//     Replacing a bad value with another bad one, or fixing one problem
//     while causing another, is not. When some problem has no path, they
//     cannot be compared, so the edit must make the configuration load.
//
// A rule spanning settings is reported under one path (tls.mode mutual
// stays a tls.mode problem until cert, key and ca are all set), so an edit
// that satisfies only part of it is not a repair: set its other settings
// first, or the setting that imposes it last.
func judgeRepair(userPath, key string, currentErr, candidateErr error, loadWithoutUser func() error) (remaining, err error) {
	if config.FieldErrorsComplete(currentErr) {
		if others := otherLayers(currentErr, userPath); len(others) > 0 {
			return nil, fmt.Errorf("the configuration already fails to load because of %s; fix that first "+
				"(config set and unset edit only %s): %w", strings.Join(others, ", "), userPath, currentErr)
		}
	} else if othersErr := loadWithoutUser(); othersErr != nil {
		return nil, fmt.Errorf("the configuration already fails to load because of a layer other than %s, named below; "+
			"fix that first (config set and unset edit only %s): %w", userPath, userPath, othersErr)
	}
	if !config.FieldErrorsComplete(currentErr) || !config.FieldErrorsComplete(candidateErr) {
		return nil, fmt.Errorf("the user config already fails to load, and this change to %s does not make it load. "+
			"Fix the errors below by hand, or the last one with config set or unset: %w", key, candidateErr)
	}
	before, after := config.FieldErrors(currentErr), config.FieldErrors(candidateErr)
	still, added := compareProblems(before, after)
	if len(added) == 0 && len(after) < len(before) {
		return candidateErr, nil
	}
	msg := "still failing: " + strings.Join(still, ", ")
	if len(added) > 0 {
		msg = "new problems: " + strings.Join(added, ", ")
	}
	return nil, fmt.Errorf("the user config already fails to load, and this change to %s does not remove one of its "+
		"problems without adding another (%s). Fix one problem per change, setting the settings a rule depends on "+
		"before the one that imposes it: %w", key, msg, candidateErr)
}

// otherLayers returns, sorted and once each, the sources of err's problems
// that another layer than the user config at userPath set.
func otherLayers(err error, userPath string) []string {
	var others []string
	for _, fe := range config.FieldErrors(err) {
		switch fe.Source {
		case userPath, config.SourceDefaults, "":
		default:
			if !slices.Contains(others, fe.Source) {
				others = append(others, fe.Source)
			}
		}
	}
	slices.Sort(others)
	return others
}

// compareProblems compares the setting paths of the problems before and
// after an edit as multisets: added lists each path after that is not
// matched by one before, still each path after that is, both sorted.
func compareProblems(before, after []*config.FieldError) (still, added []string) {
	count := make(map[string]int, len(before))
	for _, fe := range before {
		count[fe.Path]++
	}
	for _, fe := range after {
		if count[fe.Path] > 0 {
			count[fe.Path]--
			still = append(still, fe.Path)
		} else {
			added = append(added, fe.Path)
		}
	}
	slices.Sort(still)
	slices.Sort(added)
	return still, added
}

// loadFailureAdvice tells the operator how to fix the configuration that
// failed to load with profile: with config set or unset when the user
// config is the cause, or by fixing the other file the load error names.
func loadFailureAdvice(profile string) string {
	userPath := userConfigPath()
	opts := cliLoadOptions(profile, config.WithUserConfigContent([]byte{}))
	if _, err := config.NewLoader().Load(context.Background(), opts...); err == nil {
		return fmt.Sprintf("The problem is in the user config %s: fix it with "+
			"`crosscodex config set <key> <value>` or `crosscodex config unset <key>`, or edit the file", userPath)
	}
	return fmt.Sprintf("The problem is in a file other than the user config %s, named in the error; "+
		"fix that file (config set and unset edit only %s)", userPath, userPath)
}

func userConfigPath() string {
	return filepath.Join(xdgConfigHomeLocal(), "crosscodex", "config.yaml")
}

// writeFileAtomic replaces path with data: it writes a temporary file in the
// same directory, gives it the owner and mode of target (the file it
// replaces; nil for a new file, which gets mode 0600), syncs it and renames
// it over path, then syncs the directory, so readers see the old file or the
// new one and never a partial write. The temporary file is removed on any
// failure.
func writeFileAtomic(path string, data []byte, target os.FileInfo) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeIfExists(tmp.Name()))
		}
	}()

	mode := os.FileMode(0o600)
	if target != nil {
		mode = target.Mode().Perm()
		if err := matchOwner(tmp, path, target); err != nil {
			return errors.Join(err, tmp.Close())
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write config: %w", err), tmp.Close())
	}
	if err := tmp.Chmod(mode); err != nil {
		return errors.Join(fmt.Errorf("set config file mode: %w", err), tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync config: %w", err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open config directory to sync it: %w", err)
	}
	if err := d.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync config directory: %w", err), d.Close())
	}
	return d.Close()
}

// matchOwner gives tmp the owner and group of target, the file at path that
// tmp will replace, when they differ (as when root edits another user's
// config), so the rename does not hand the file to whoever ran the command.
// Without the privilege to do so it refuses, before path is touched.
func matchOwner(tmp *os.File, path string, target os.FileInfo) error {
	want, ok := target.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read the owner of %s on this platform", path)
	}
	info, err := tmp.Stat()
	if err != nil {
		return fmt.Errorf("stat temporary config: %w", err)
	}
	have, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read the owner of the temporary config on this platform")
	}
	if have.Uid == want.Uid && have.Gid == want.Gid {
		return nil
	}
	if err := tmp.Chown(int(want.Uid), int(want.Gid)); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("%s is owned by %d:%d; run config set as that user (the file is unchanged): %w",
				path, want.Uid, want.Gid, err)
		}
		return fmt.Errorf("give the new config the owner of %s: %w", path, err)
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove temporary config file %s: %w", path, err)
	}
	return nil
}

// setYAMLPath sets the key path names, below the mapping node, to value,
// creating missing (or null) sections on the way.
func setYAMLPath(node *yaml.Node, path []string, value string) error {
	section, err := sectionFor(node, path, true)
	if err != nil {
		return err
	}
	key := path[len(path)-1]
	if i := keyIndex(section, key); i >= 0 {
		old := section.Content[i+1]
		if isSection(old) {
			dotted := config.FormatPath(path...)
			return fmt.Errorf("%s is a section or list, not a value, so config set cannot replace it; "+
				"set one of its keys instead (%s.<key>), or edit the file by hand: %w", dotted, dotted, errNotAValue)
		}
		// A new node, so the old value's tag (say !!int) does not stick to
		// the new one; the comments stay.
		// refuseSharedAnchors has made sure no alias refers to an anchor
		// here, so keeping it cannot leave an alias dangling.
		section.Content[i+1] = &yaml.Node{
			Kind: yaml.ScalarNode, Value: value, Anchor: old.Anchor,
			HeadComment: old.HeadComment, LineComment: old.LineComment, FootComment: old.FootComment,
		}
		return nil
	}
	section.Content = append(section.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value})
	return nil
}

// unsetYAMLPath removes the key path names from below the mapping node, the
// root of the user config at userPath.
func unsetYAMLPath(node *yaml.Node, path []string, userPath string) error {
	section, err := sectionFor(node, path, false)
	if err != nil {
		return err
	}
	i := -1
	if section != nil {
		i = keyIndex(section, path[len(path)-1])
	}
	if i < 0 {
		return fmt.Errorf("%s is not set in %s, so there is nothing to unset; it may come from another layer "+
			"(the system config, a drop-in, a profile or the environment): %w",
			config.FormatPath(path...), userPath, errKeyNotSet)
	}
	section.Content = append(section.Content[:i], section.Content[i+2:]...)
	return nil
}

// sectionFor returns the mapping that holds the last key of path, walking
// the mapping node along the keys before it. With create, a missing or null
// section is created; without, sectionFor returns nil for one. An alias or a
// value on the way is refused with errNotASection.
func sectionFor(node *yaml.Node, path []string, create bool) (*yaml.Node, error) {
	if len(path) == 0 || path[0] == "" {
		return nil, fmt.Errorf("empty path")
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping node: the user config must hold a mapping of sections")
	}
	for depth, key := range path[:len(path)-1] {
		dotted := config.FormatPath(path[:depth+1]...)
		i := keyIndex(node, key)
		if i < 0 {
			if !create {
				return nil, nil
			}
			child := &yaml.Node{Kind: yaml.MappingNode}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, child)
			node = child
			continue
		}
		child := node.Content[i+1]
		switch {
		case child.Kind == yaml.AliasNode:
			return nil, fmt.Errorf("%s is an alias to another section; edit the anchor or the file by hand: %w",
				dotted, errNotASection)
		case child.Kind == yaml.ScalarNode && child.ShortTag() == "!!null":
			if !create {
				return nil, nil
			}
			child = &yaml.Node{Kind: yaml.MappingNode, Anchor: child.Anchor,
				HeadComment: child.HeadComment, LineComment: child.LineComment, FootComment: child.FootComment}
			node.Content[i+1] = child
		case child.Kind != yaml.MappingNode:
			return nil, fmt.Errorf("%s is a value, not a section, so it has no key %s; change %s itself, "+
				"or edit the file by hand: %w", dotted, path[depth+1], dotted, errNotASection)
		}
		node = child
	}
	return node, nil
}

// refuseSharedAnchors refuses an edit at path, below the mapping root, when
// it would change what an alias in the file refers to: a section on the way
// or the value at path carries an anchor some alias refers to, or the value
// at path is a section (which unset removes) holding an anchor that an alias
// outside it refers to. Either way the change would also change, or leave
// dangling, every place the alias stands for, such as another tenant's
// inherited settings. Aliases inside a removed section go with it, and an
// anchor no alias refers to is harmless and kept.
func refuseSharedAnchors(root *yaml.Node, path []string) error {
	refs := aliasPaths(root)
	shared := func(n *yaml.Node, keys []string, aliases []string) error {
		return fmt.Errorf("%s carries the anchor &%s, which the alias at %s refers to, so this change would "+
			"also change what the alias stands for; edit the file by hand: %w",
			config.FormatPath(keys...), n.Anchor, strings.Join(aliases, ", "), errNotASection)
	}
	node := root
	for depth, key := range path {
		i := keyIndex(node, key)
		if i < 0 {
			return nil
		}
		child := node.Content[i+1]
		keys := path[:depth+1]
		if child.Anchor != "" && len(refs[child]) > 0 {
			return shared(child, keys, refs[child])
		}
		if depth == len(path)-1 {
			return refuseAnchorsBelow(child, keys, refs, shared)
		}
		if child.Kind != yaml.MappingNode {
			return nil
		}
		node = child
	}
	return nil
}

// refuseAnchorsBelow checks the nodes below n, the value at keys that an
// edit replaces or removes, for an anchor that an alias outside n refers to.
func refuseAnchorsBelow(n *yaml.Node, keys []string, refs map[*yaml.Node][]string,
	shared func(*yaml.Node, []string, []string) error,
) error {
	prefix := config.FormatPath(keys...)
	outside := func(aliases []string) []string {
		var out []string
		for _, a := range aliases {
			if a != prefix && !strings.HasPrefix(a, prefix+".") && !strings.HasPrefix(a, prefix+"[") {
				out = append(out, a)
			}
		}
		return out
	}
	var walk func(n *yaml.Node, keys []string) error
	walk = func(n *yaml.Node, keys []string) error {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				child := n.Content[i+1]
				childKeys := append(slices.Clone(keys), n.Content[i].Value)
				if aliases := outside(refs[child]); child.Anchor != "" && len(aliases) > 0 {
					return shared(child, childKeys, aliases)
				}
				if err := walk(child, childKeys); err != nil {
					return err
				}
			}
		case yaml.SequenceNode:
			for _, item := range n.Content {
				if aliases := outside(refs[item]); item.Anchor != "" && len(aliases) > 0 {
					return shared(item, keys, aliases)
				}
				if err := walk(item, keys); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(n, keys)
}

// aliasPaths maps each node an alias in the tree below n refers to onto the
// config paths of those aliases (a merge key's is "<section>.<<").
func aliasPaths(n *yaml.Node) map[*yaml.Node][]string {
	refs := make(map[*yaml.Node][]string)
	var walk func(n *yaml.Node, keys []string, path string)
	walk = func(n *yaml.Node, keys []string, path string) {
		switch n.Kind {
		case yaml.AliasNode:
			refs[n.Alias] = append(refs[n.Alias], path)
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				child := append(slices.Clone(keys), n.Content[i].Value)
				walk(n.Content[i+1], child, config.FormatPath(child...))
			}
		case yaml.SequenceNode:
			for i, item := range n.Content {
				walk(item, keys, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(n, nil, "")
	return refs
}

// keyIndex returns the index in mapping m's Content of the key named key,
// or -1. Merge keys are not keys a path can name.
func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key && k.Value != "<<" {
			return i
		}
	}
	return -1
}

// isSection reports whether n is a mapping or a sequence, directly or
// through an alias.
func isSection(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode
}
