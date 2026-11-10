package prdash

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var ErrConfigValidation = errors.New("invalid repository configuration")

// ConfigError locates a syntax, schema, or policy error in repository settings.
type ConfigError struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason"`
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("%s:%d:%d (%s): %s", e.File, e.Line, e.Column, e.Field, e.Reason)
}
func (e *ConfigError) Unwrap() error { return ErrConfigValidation }

type ReviewConfig struct {
	Agent            string    `json:"agent" yaml:"agent"`
	Session          string    `json:"session" yaml:"session"`
	Model            string    `json:"model,omitempty" yaml:"model"`
	ModelTier        ModelTier `json:"model_tier,omitempty" yaml:"model_tier"`
	InstructionsFile string    `json:"instructions_file,omitempty" yaml:"instructions_file"`
}
type FindingPolicies struct {
	AI       string `json:"ai" yaml:"ai"`
	Human    string `json:"human" yaml:"human"`
	Question string `json:"question" yaml:"question"`
}
type RereviewConfig struct {
	Gate      string `json:"gate" yaml:"gate"`
	Agent     string `json:"agent,omitempty" yaml:"agent"`
	Session   string `json:"session,omitempty" yaml:"session"`
	MaxRounds int    `json:"max_rounds" yaml:"max_rounds"`
	StopOn    string `json:"stop_on" yaml:"stop_on"`
}
type ReviewResponseConfig struct {
	Steps            []string        `json:"steps" yaml:"steps"`
	Agent            string          `json:"agent,omitempty" yaml:"agent"`
	Session          string          `json:"session" yaml:"session"`
	InstructionsFile string          `json:"instructions_file,omitempty" yaml:"instructions_file"`
	Verify           []string        `json:"verify" yaml:"verify"`
	FindingPolicies  FindingPolicies `json:"finding_policies" yaml:"finding_policies"`
	Fix              string          `json:"fix" yaml:"fix"`
	Test             string          `json:"test" yaml:"test"`
	Commit           string          `json:"commit" yaml:"commit"`
	Push             string          `json:"push" yaml:"push"`
	Reply            string          `json:"reply" yaml:"reply"`
	Resolve          string          `json:"resolve" yaml:"resolve"`
	Rereview         RereviewConfig  `json:"rereview" yaml:"rereview"`
}
type RepoConfig struct {
	Version        int                  `json:"version" yaml:"version"`
	Review         ReviewConfig         `json:"review" yaml:"review"`
	ReviewResponse ReviewResponseConfig `json:"review_response" yaml:"review_response"`
}

// EffectiveConfig is a value snapshot; instruction contents are captured rather
// than read again by a consumer after a reload.
type EffectiveConfig struct {
	Config          RepoConfig        `json:"config"`
	RepoRoot        string            `json:"repo_root"`
	Source          string            `json:"source"`
	SourcePath      string            `json:"source_path"`
	DefaultedFields []string          `json:"defaulted_fields"`
	Instructions    map[string]string `json:"instructions"`
	Revision        string            `json:"revision"`
	LoadedAt        time.Time         `json:"loaded_at"`
}

func DefaultRepoConfig() RepoConfig {
	return RepoConfig{Version: 1, Review: ReviewConfig{Agent: "sbnn", Session: "separate"}, ReviewResponse: ReviewResponseConfig{
		Steps: []string{"fix", "test", "commit", "push", "reply", "resolve", "rereview"}, Session: "working", Verify: []string{}, FindingPolicies: FindingPolicies{AI: "auto", Human: "prepare_reply", Question: "human_decide"},
		Fix: "auto", Test: "auto", Commit: "confirm", Push: "confirm", Reply: "confirm", Resolve: "confirm",
		Rereview: RereviewConfig{Gate: "confirm", MaxRounds: 1, StopOn: "approved"},
	}}
}

// FindRepositoryRoot respects .git boundaries, including linked worktrees. A
// nested prdash.yaml never overrides the repository-root configuration.
func FindRepositoryRoot(ctx context.Context, start string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	root, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for dir := root; ; dir = filepath.Dir(dir) {
		if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil {
			return dir, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	// Bare repositories and unusual git layouts are resolved by git itself.
	if dir, gitErr := runGit(ctx, root, "rev-parse", "--show-toplevel"); gitErr == nil && dir != "" {
		return dir, nil
	}
	return root, nil
}

// LoadRepoConfig reads only local configuration and instruction files. It does
// not run verification commands, select a live agent, or perform external actions.
func LoadRepoConfig(ctx context.Context, start string) (EffectiveConfig, error) {
	root, err := FindRepositoryRoot(ctx, start)
	if err != nil {
		return EffectiveConfig{}, err
	}
	path := filepath.Join(root, "prdash.yaml")
	cfg := DefaultRepoConfig()
	result := EffectiveConfig{Config: cfg, RepoRoot: root, Source: "defaults", SourcePath: path, Instructions: map[string]string{}, LoadedAt: time.Now().UTC()}
	explicit := map[string]*yaml.Node{}
	data, readErr := readConfigFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return EffectiveConfig{}, fmt.Errorf("read config: %w", readErr)
	}
	if readErr == nil {
		result.Source = "file"
		dec := yaml.NewDecoder(bytes.NewReader(data))
		var doc yaml.Node
		if decodeErr := dec.Decode(&doc); decodeErr != nil && decodeErr != io.EOF {
			line := 1
			_, _ = fmt.Sscanf(decodeErr.Error(), "yaml: line %d:", &line)
			return EffectiveConfig{}, &ConfigError{File: path, Line: line, Column: 1, Reason: decodeErr.Error()}
		}
		var extra yaml.Node
		if extraErr := dec.Decode(&extra); extraErr != io.EOF {
			return EffectiveConfig{}, &ConfigError{File: path, Line: extra.Line, Column: extra.Column, Reason: "exactly one YAML document is allowed"}
		}
		if len(doc.Content) > 0 {
			node := doc.Content[0]
			if validateErr := validateConfigNode(path, node, reflect.TypeFor[RepoConfig](), "", explicit); validateErr != nil {
				return EffectiveConfig{}, validateErr
			}
			if decodeErr := node.Decode(&cfg); decodeErr != nil {
				return EffectiveConfig{}, &ConfigError{File: path, Line: node.Line, Column: node.Column, Reason: decodeErr.Error()}
			}
		}
	}
	if err := validateRepoConfig(path, cfg, explicit); err != nil {
		return EffectiveConfig{}, err
	}
	for field, relative := range map[string]string{"review.instructions_file": cfg.Review.InstructionsFile, "review_response.instructions_file": cfg.ReviewResponse.InstructionsFile} {
		if relative == "" {
			continue
		}
		content, fileErr := readInstructions(root, relative)
		if fileErr != nil {
			return EffectiveConfig{}, configFieldError(path, field, fileErr.Error(), explicit)
		}
		result.Instructions[field] = content
	}
	result.Config = cfg
	for _, field := range configLeafFields(reflect.TypeFor[RepoConfig](), "") {
		if _, ok := explicit[field]; !ok {
			result.DefaultedFields = append(result.DefaultedFields, field)
		}
	}
	slices.Sort(result.DefaultedFields)
	// The source and default provenance are part of the audit identity, too.
	// LoadedAt must not make identical reads look like different policies.
	result.LoadedAt = time.Time{}
	encoded, err := json.Marshal(result)
	if err != nil {
		return EffectiveConfig{}, err
	}
	sum := sha256.Sum256(encoded)
	result.Revision = hex.EncodeToString(sum[:])
	result.LoadedAt = time.Now().UTC()
	return result, nil
}

func readConfigFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, statErr := f.Stat()
	if statErr != nil {
		return nil, statErr
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s exceeds 1 MiB", path)
	}
	return data, nil
}

func readInstructions(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", errors.New("instruction path must be repository-relative")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("instruction path must stay within the repository")
	}
	content, err := readConfigFile(resolved)
	return string(content), err
}

func validateConfigNode(file string, node *yaml.Node, typ reflect.Type, path string, explicit map[string]*yaml.Node) error {
	fail := func(reason string) error {
		return &ConfigError{File: file, Line: node.Line, Column: node.Column, Field: path, Reason: reason}
	}
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return fail("YAML aliases and anchors are not supported")
	}
	switch typ.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return fail("expected a mapping")
		}
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			field := key.Value
			if path != "" {
				field = path + "." + field
			}
			if key.Tag != "!!str" || seen[key.Value] {
				return &ConfigError{File: file, Line: key.Line, Column: key.Column, Field: field, Reason: "duplicate or invalid key"}
			}
			seen[key.Value] = true
			var fieldType reflect.Type
			for j := 0; j < typ.NumField(); j++ {
				f := typ.Field(j)
				if f.Tag.Get("yaml") == key.Value {
					fieldType = f.Type
					break
				}
			}
			if fieldType == nil {
				return &ConfigError{File: file, Line: key.Line, Column: key.Column, Field: field, Reason: "unknown key"}
			}
			explicit[field] = value
			if err := validateConfigNode(file, value, fieldType, field, explicit); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if node.Kind != yaml.SequenceNode {
			return fail("expected a list")
		}
		for _, item := range node.Content {
			if err := validateConfigNode(file, item, typ.Elem(), path, explicit); err != nil {
				return err
			}
		}
	case reflect.Int:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
			return fail("expected an integer")
		}
	case reflect.String:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return fail("expected a string")
		}
	default:
		return fail("unsupported schema type")
	}
	return nil
}

func configLeafFields(typ reflect.Type, prefix string) []string {
	var fields []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := field.Tag.Get("yaml")
		if prefix != "" {
			name = prefix + "." + name
		}
		if field.Type.Kind() == reflect.Struct {
			fields = append(fields, configLeafFields(field.Type, name)...)
		} else {
			fields = append(fields, name)
		}
	}
	return fields
}

func configFieldError(file, field, reason string, explicit map[string]*yaml.Node) error {
	line, column := 1, 1
	if node := explicit[field]; node != nil {
		line, column = node.Line, node.Column
	}
	return &ConfigError{File: file, Line: line, Column: column, Field: field, Reason: reason}
}

func validateRepoConfig(file string, cfg RepoConfig, explicit map[string]*yaml.Node) error {
	fail := func(field, reason string) error { return configFieldError(file, field, reason, explicit) }
	if cfg.Version != 1 {
		return fail("version", "only schema version 1 is supported")
	}
	if strings.TrimSpace(cfg.Review.Agent) == "" {
		return fail("review.agent", "must not be empty")
	}
	for field, value := range map[string]string{"review.session": cfg.Review.Session, "review_response.session": cfg.ReviewResponse.Session} {
		if !slices.Contains([]string{"working", "separate", "controller", "dedicated"}, value) {
			return fail(field, "expected working, separate, controller, or dedicated")
		}
	}
	if cfg.Review.Model != "" && cfg.Review.ModelTier != "" {
		return fail("review.model_tier", "model and model_tier are mutually exclusive")
	}
	if cfg.Review.ModelTier != "" && !ValidModelTier(cfg.Review.ModelTier) {
		return fail("review.model_tier", "expected pine, bamboo, or plum")
	}
	rr := cfg.ReviewResponse
	for field, value := range map[string]string{"fix": rr.Fix, "test": rr.Test, "commit": rr.Commit, "push": rr.Push, "reply": rr.Reply, "resolve": rr.Resolve} {
		if !slices.Contains([]string{"auto", "confirm", "off"}, value) {
			return fail("review_response."+field, "expected auto, confirm, or off")
		}
	}
	for field, value := range map[string]string{"ai": rr.FindingPolicies.AI, "human": rr.FindingPolicies.Human, "question": rr.FindingPolicies.Question} {
		if !slices.Contains([]string{"auto", "prepare_reply", "human_decide"}, value) {
			return fail("review_response.finding_policies."+field, "expected auto, prepare_reply, or human_decide")
		}
	}
	for _, command := range rr.Verify {
		if strings.TrimSpace(command) == "" {
			return fail("review_response.verify", "verification commands must not be blank")
		}
	}
	if rr.Test == "off" && len(rr.Verify) > 0 {
		return fail("review_response.verify", "verification commands contradict test: off")
	}
	if rr.Commit == "off" && rr.Push == "auto" {
		return fail("review_response.push", "automatic push contradicts commit: off")
	}
	if !slices.Contains([]string{"auto", "confirm", "off"}, rr.Rereview.Gate) {
		return fail("review_response.rereview.gate", "expected auto, confirm, or off")
	}
	if err := validateResponseOrder(rr); err != nil {
		return fail("review_response.steps", err.Error())
	}
	if rr.Rereview.MaxRounds < 0 || rr.Rereview.MaxRounds > 20 {
		return fail("review_response.rereview.max_rounds", "must be between 0 and 20")
	}
	if rr.Rereview.Session != "" && !slices.Contains([]string{"working", "separate", "controller", "dedicated"}, rr.Rereview.Session) {
		return fail("review_response.rereview.session", "unsupported session role")
	}
	if !slices.Contains([]string{"approved", "no_open_comments"}, rr.Rereview.StopOn) {
		return fail("review_response.rereview.stop_on", "expected approved or no_open_comments")
	}
	return nil
}

func cloneEffectiveConfig(in EffectiveConfig) EffectiveConfig {
	out := in
	out.Config.ReviewResponse.Verify = slices.Clone(in.Config.ReviewResponse.Verify)
	out.Config.ReviewResponse.Steps = slices.Clone(in.Config.ReviewResponse.Steps)
	out.DefaultedFields = slices.Clone(in.DefaultedFields)
	out.Instructions = maps.Clone(in.Instructions)
	return out
}

// ConfigLoadError reports the last load/reload failure. A failed reload keeps
// the last good value available for inspection but blocks new work dispatch.
func (s *Service) ConfigLoadError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.configErr
}
func (s *Service) EffectiveConfig() EffectiveConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneEffectiveConfig(s.config)
}

// ReloadConfig explicitly reloads this service's repository root. It never
// retargets the service to a PR worktree or changes already queued requests.
func (s *Service) ReloadConfig(ctx context.Context) (EffectiveConfig, error) {
	s.mu.Lock()
	root := s.config.RepoRoot
	if root == "" {
		root = s.workDir
	}
	cfg, err := LoadRepoConfig(ctx, root)
	s.configErr = err
	if err == nil {
		s.config = cfg
	}
	result := cloneEffectiveConfig(s.config)
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.broadcast(snap)
	return result, err
}

func (s *Service) configErrorStringLocked() string {
	if s.configErr == nil {
		return ""
	}
	return s.configErr.Error()
}

func cloneSessionMessage(in SessionMessage) SessionMessage {
	out := in
	if in.AppliedConfig != nil {
		cfg := cloneEffectiveConfig(*in.AppliedConfig)
		out.AppliedConfig = &cfg
	}
	return out
}
func cloneThreadMessage(in ThreadMessage) ThreadMessage {
	out := in
	if in.AppliedConfig != nil {
		cfg := cloneEffectiveConfig(*in.AppliedConfig)
		out.AppliedConfig = &cfg
	}
	return out
}
