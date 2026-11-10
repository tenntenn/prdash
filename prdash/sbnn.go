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
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SBNNCommentRequest describes a pull request comment dispatched through the local sbnn review tool.
type SBNNCommentRequest struct {
	Repository   string `json:"repository"`
	PRNumber     int    `json:"pr_number"`
	Branch       string `json:"branch"`
	BaseBranch   string `json:"base_branch,omitempty"`
	HeadSHA      string `json:"head_sha,omitempty"`
	Worktree     string `json:"worktree"`
	WorktreePath string `json:"worktree_path,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	Line         int    `json:"line,omitempty"`
	Body         string `json:"body"`
}

// SBNNCommentResult contains the formatted comment record produced by sbnn.
type SBNNCommentResult struct {
	CommentID      string    `json:"comment_id"`
	CommandLine    string    `json:"command_line"`
	Formatted      string    `json:"formatted"`
	GroupURL       string    `json:"group_url,omitempty"`
	ReviewedCommit string    `json:"reviewed_commit,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// SBNNReviewRequest describes an automated review request sent to sbnn.
type SBNNReviewRequest struct {
	Repository   string `json:"repository"`
	PRNumber     int    `json:"pr_number"`
	Branch       string `json:"branch"`
	BaseBranch   string `json:"base_branch"`
	HeadSHA      string `json:"head_sha,omitempty"`
	Worktree     string `json:"worktree"`
	WorktreePath string `json:"worktree_path,omitempty"`
	Focus        string `json:"focus,omitempty"`
}

// SBNNReviewResult contains the structured findings and completion state returned by sbnn.
type SBNNReviewResult struct {
	Stale          bool      `json:"stale,omitempty"`
	CommandLine    string    `json:"command_line"`
	Summary        string    `json:"summary"`
	Findings       []string  `json:"findings"`
	GroupURL       string    `json:"group_url,omitempty"`
	ReviewedCommit string    `json:"reviewed_commit,omitempty"`
	Reviewed       bool      `json:"reviewed"`
	Verdict        string    `json:"verdict"` // "approved", "changes_requested", "commented", "pending"
	CreatedAt      time.Time `json:"created_at"`
}

// SBNNReviewer defines the integration boundary for the local sbnn review tool.
type SBNNReviewer interface {
	SubmitComment(ctx context.Context, req SBNNCommentRequest) (SBNNCommentResult, error)
	RunReview(ctx context.Context, req SBNNReviewRequest) (SBNNReviewResult, error)
}

type deliveredDiffRecord struct {
	group     sbnnGroup
	commitSHA string
	diffHash  string
	diffText  string
	groupURL  string
}

// CLISBNNClient invokes the real `sbnn` CLI binary on PATH.
type CLISBNNClient struct {
	Bin string
	Dir string
}

// Serialize check-and-send within the process, including separate clients.
var sbnnDeliveryMu sync.Mutex

// NewCLISBNNClient returns a CLISBNNClient configured with the given binary name and working directory.
func NewCLISBNNClient(bin, dir string) *CLISBNNClient {
	if strings.TrimSpace(bin) == "" {
		bin = "sbnn"
	}
	return &CLISBNNClient{
		Bin: bin,
		Dir: dir,
	}
}

// SubmitComment sends a comment to the repository-scoped PR review group.
func (c *CLISBNNClient) SubmitComment(ctx context.Context, req SBNNCommentRequest) (SBNNCommentResult, error) {
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return SBNNCommentResult{}, errors.New("sbnn comment body is required")
	}

	sbnnDeliveryMu.Lock()
	defer sbnnDeliveryMu.Unlock()
	target := c.reviewTarget(req.Repository, req.PRNumber, req.WorktreePath)
	base := req.BaseBranch
	if strings.TrimSpace(base) == "" {
		base = "main"
	}
	rec, err := c.ensurePRDiffSent(ctx, req.Repository, req.PRNumber, base, req.Branch, req.HeadSHA, req.WorktreePath, target)
	if err != nil {
		return SBNNCommentResult{}, err
	}

	filePath := strings.TrimSpace(req.FilePath)
	if filePath == "" {
		filePath = firstDiffFilePath(rec.diffText)
		if filePath == "" {
			filePath = "PR"
		}
	}
	line := req.Line
	if line <= 0 {
		line = 1
	}
	anchor := fmt.Sprintf("%s:%d", filePath, line)

	workDir := strings.TrimSpace(req.WorktreePath)
	if workDir == "" {
		workDir = c.Dir
	}

	args := []string{"comment", anchor, "-m", body, "--author", "prdash", "--target", target}
	cmdLine := c.Bin + " " + strings.Join(args, " ")

	cmd := exec.CommandContext(ctx, c.Bin, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		return SBNNCommentResult{}, fmt.Errorf("%s: %w (%s)", cmdLine, runErr, strings.TrimSpace(string(out)))
	}

	now := time.Now().UTC()
	return SBNNCommentResult{
		CommentID:      fmt.Sprintf("sbnn-cmt-%d-%d", req.PRNumber, now.UnixNano()%100000),
		CommandLine:    cmdLine,
		Formatted:      fmt.Sprintf("[sbnn %s] %s", anchor, body),
		GroupURL:       rec.groupURL,
		ReviewedCommit: rec.commitSHA,
		CreatedAt:      now,
	}, nil
}

func (c *CLISBNNClient) ensurePRDiffSent(ctx context.Context, repo string, prNumber int, base, head, headSHA, worktreePath, target string) (deliveredDiffRecord, error) {
	if strings.TrimSpace(base) == "" {
		base = "main"
	}
	if strings.TrimSpace(head) == "" {
		head = "HEAD"
	}
	workDir := strings.TrimSpace(worktreePath)
	if workDir == "" {
		workDir = c.Dir
	}

	commitRef := head
	pinned := strings.TrimSpace(headSHA) != ""
	if pinned {
		commitRef = strings.TrimSpace(headSHA)
	}
	commitSHA, resolveErr := runGit(ctx, workDir, "rev-parse", "--verify", "--end-of-options", commitRef+"^{commit}")
	if pinned && resolveErr != nil {
		return deliveredDiffRecord{}, fmt.Errorf("resolve requested commit %s: %w", commitRef, resolveErr)
	}
	diffHead := head
	if resolveErr == nil {
		diffHead = commitSHA
	}
	diffOut, diffErr := runGit(ctx, workDir, "diff", base+"..."+diffHead, "--")
	if pinned && diffErr != nil {
		return deliveredDiffRecord{}, fmt.Errorf("acquire diff for requested commit %s: %w", commitSHA, diffErr)
	}

	if !pinned && (diffErr != nil || strings.TrimSpace(diffOut) == "") && prNumber > 0 {
		before, err := readGitHubDiffTarget(ctx, workDir, repo, prNumber)
		if err != nil {
			return deliveredDiffRecord{}, err
		}
		ghArgs := []string{"pr", "diff", strconv.Itoa(prNumber)}
		if strings.TrimSpace(repo) != "" {
			ghArgs = append(ghArgs, "--repo", strings.TrimSpace(repo))
		}
		ghCmd := exec.CommandContext(ctx, "gh", ghArgs...)
		ghCmd.Dir = workDir
		out, ghErr := ghCmd.Output()
		if ghErr != nil {
			return deliveredDiffRecord{}, fmt.Errorf("acquire GitHub diff for %s: %w", target, ghErr)
		}
		after, err := readGitHubDiffTarget(ctx, workDir, repo, prNumber)
		if err != nil {
			return deliveredDiffRecord{}, err
		}
		if before != after {
			return deliveredDiffRecord{}, fmt.Errorf("GitHub diff target changed while reading %s; retry the review", target)
		}
		diffOut = string(out)
		commitSHA = before.HeadSHA
		diffErr = nil
	}

	if strings.TrimSpace(diffOut) == "" {
		if diffErr != nil {
			return deliveredDiffRecord{}, fmt.Errorf("acquire diff for %s (%s...%s): %w", target, base, head, diffErr)
		}
		return deliveredDiffRecord{}, fmt.Errorf("acquire diff for %s (%s...%s): diff is empty", target, base, head)
	}

	sum := sha256.Sum256([]byte(diffOut))
	diffHash := hex.EncodeToString(sum[:16])

	serverURL, group, err := c.currentGroup(ctx, workDir, target)
	if err != nil {
		return deliveredDiffRecord{}, err
	}
	rec := deliveredDiffRecord{commitSHA: commitSHA, diffHash: diffHash, diffText: diffOut}
	if group != nil && latestMatches(*group, rec) {
		rec.group = *group
		rec.groupURL = serverURL + "/" + url.PathEscape(target)
		return rec, nil
	}

	sendArgs := []string{
		"--target", target,
		"--title", fmt.Sprintf("PR #%d (%s)", prNumber, head),
		"--label", "pr=" + strconv.Itoa(prNumber),
		"--label", "prdash_head=" + commitSHA,
		"--label", "prdash_diff=" + diffHash,
		"--no-open",
		"--json",
	}
	sendCmd := exec.CommandContext(ctx, c.Bin, sendArgs...)
	if workDir != "" {
		sendCmd.Dir = workDir
	}
	sendCmd.Stdin = strings.NewReader(diffOut)
	var sendStdout, sendStderr bytes.Buffer
	sendCmd.Stdout = &sendStdout
	sendCmd.Stderr = &sendStderr
	if err := sendCmd.Run(); err != nil {
		return deliveredDiffRecord{}, fmt.Errorf("%s %s: %w (%s)", c.Bin, strings.Join(sendArgs, " "), err, strings.TrimSpace(sendStderr.String()))
	}

	var sendPayload struct {
		URL   string `json:"url"`
		Group string `json:"group"`
		Diff  struct {
			ID string `json:"id"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(sendStdout.Bytes(), &sendPayload); err != nil {
		return deliveredDiffRecord{}, fmt.Errorf("parse sbnn delivery for %s: %w", target, err)
	}
	if sendPayload.Group != target || strings.TrimSpace(sendPayload.URL) == "" {
		return deliveredDiffRecord{}, fmt.Errorf("sbnn delivery for %s: missing URL or mismatched group %q", target, sendPayload.Group)
	}

	if sendPayload.Diff.ID == "" {
		return deliveredDiffRecord{}, errors.New("sbnn delivery: missing diff ID")
	}
	baseURL, err := sbnnBaseURL(sendPayload.URL)
	if err != nil {
		return deliveredDiffRecord{}, err
	}
	group, err = readSBNNGroup(ctx, baseURL, target)
	if err != nil {
		return deliveredDiffRecord{}, err
	}
	if group == nil || !latestMatches(*group, rec) || group.Diffs[len(group.Diffs)-1].ID != sendPayload.Diff.ID {
		return deliveredDiffRecord{}, errors.New("sbnn delivery: sent diff is not the current review target")
	}
	rec.group = *group
	rec.groupURL = sendPayload.URL
	return rec, nil
}

type githubDiffTarget struct {
	HeadSHA string `json:"headRefOid"`
	BaseSHA string `json:"baseRefOid"`
}

func readGitHubDiffTarget(ctx context.Context, dir, repo string, number int) (githubDiffTarget, error) {
	args := []string{"pr", "view", strconv.Itoa(number), "--json", "headRefOid,baseRefOid"}
	if strings.TrimSpace(repo) != "" {
		args = append(args, "--repo", strings.TrimSpace(repo))
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return githubDiffTarget{}, fmt.Errorf("read GitHub diff target for PR #%d: %w", number, err)
	}
	var target githubDiffTarget
	if err := json.Unmarshal(out, &target); err != nil {
		return githubDiffTarget{}, fmt.Errorf("parse GitHub diff target: %w", err)
	}
	for _, sha := range []string{target.HeadSHA, target.BaseSHA} {
		decoded, err := hex.DecodeString(sha)
		if err != nil || (len(decoded) != 20 && len(decoded) != 32) {
			return githubDiffTarget{}, fmt.Errorf("invalid GitHub diff target commit %q", sha)
		}
	}
	return target, nil
}

func firstDiffFilePath(diffText string) string {
	for line := range strings.SplitSeq(diffText, "\n") {
		if after, ok := strings.CutPrefix(line, "+++ b/"); ok {
			p := strings.TrimSpace(after)
			if p != "" && p != "/dev/null" {
				return p
			}
		}
	}
	return ""
}

type sbnnCommentEntry struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Author    string `json:"author"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Body      string `json:"body"`
	Resolved  bool   `json:"resolved"`
}

type sbnnDiff struct {
	ID        string            `json:"id"`
	Raw       string            `json:"raw"`
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"createdAt"`
}

type sbnnGroup struct {
	Name          string             `json:"name"`
	Diffs         []sbnnDiff         `json:"diffs"`
	Comments      []sbnnCommentEntry `json:"comments"`
	ReviewedAt    time.Time          `json:"reviewedAt"`
	ReviewVerdict string             `json:"reviewVerdict"`
}

func (c *CLISBNNClient) reviewTarget(repo string, number int, dir string) string {
	identity := strings.ToLower(strings.TrimSpace(repo))
	if identity == "" {
		if dir == "" {
			dir = c.Dir
		}
		if common, err := runGit(context.Background(), dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
			identity = common
		} else {
			identity, _ = filepath.Abs(dir)
		}
	}
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("prdash-%x-pr-%d", sum[:12], number)
}

func sbnnBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse sbnn URL: %w", err)
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme != "http" || u.User != nil || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return "", fmt.Errorf("sbnn URL must identify a local HTTP server: %q", raw)
	}
	return "http://" + u.Host, nil
}

func (c *CLISBNNClient) currentGroup(ctx context.Context, dir, target string) (string, *sbnnGroup, error) {
	cmd := exec.CommandContext(ctx, c.Bin, "--status", "--json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("sbnn status: %w", err)
	}
	var status struct {
		App    string `json:"app"`
		Status string `json:"status"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", nil, fmt.Errorf("parse sbnn status: %w", err)
	}
	base, err := sbnnBaseURL(status.URL)
	if err != nil {
		return "", nil, err
	}
	if status.Status == "not running" {
		return base, nil, nil
	}
	if status.App != "sbnn" {
		return "", nil, errors.New("sbnn status: unexpected application")
	}
	group, err := readSBNNGroup(ctx, base, target)
	return base, group, err
}

func readSBNNGroup(ctx context.Context, base, target string) (*sbnnGroup, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/_/api/groups/"+url.PathEscape(target), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: rejectSBNNRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read sbnn group: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read sbnn group: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read sbnn group: %w", err)
	}
	if len(data) > 64<<20 {
		return nil, errors.New("sbnn group response exceeds 64 MiB")
	}
	var group sbnnGroup
	if err := json.Unmarshal(data, &group); err != nil {
		return nil, fmt.Errorf("parse sbnn group: %w", err)
	}
	if group.Name != target || group.Diffs == nil {
		return nil, errors.New("sbnn group: missing diffs or mismatched name")
	}
	return &group, nil
}

func rejectSBNNRedirect(_ *http.Request, _ []*http.Request) error {
	return errors.New("sbnn redirect is not supported")
}

func latestMatches(group sbnnGroup, rec deliveredDiffRecord) bool {
	if len(group.Diffs) == 0 {
		return false
	}
	latest := group.Diffs[len(group.Diffs)-1]
	// sbnn may omit raw patch text from the group API; labels identify delivered content.
	sum := sha256.Sum256([]byte(latest.Raw))
	return latest.ID != "" && latest.Labels["prdash_head"] == rec.commitSHA && latest.Labels["prdash_diff"] == rec.diffHash && (latest.Raw == "" || hex.EncodeToString(sum[:16]) == rec.diffHash)
}

// RunReview delivers a diff and reads the matching group's actual submission.
// It never submits or approves a review on behalf of a reviewer.
func (c *CLISBNNClient) RunReview(ctx context.Context, req SBNNReviewRequest) (SBNNReviewResult, error) {
	sbnnDeliveryMu.Lock()
	defer sbnnDeliveryMu.Unlock()
	target := c.reviewTarget(req.Repository, req.PRNumber, req.WorktreePath)
	rec, err := c.ensurePRDiffSent(ctx, req.Repository, req.PRNumber, req.BaseBranch, req.Branch, req.HeadSHA, req.WorktreePath, target)
	if err != nil {
		return SBNNReviewResult{}, err
	}
	group := rec.group
	reviewed := !group.ReviewedAt.IsZero()
	for _, diff := range group.Diffs {
		if diff.CreatedAt.IsZero() || diff.CreatedAt.After(group.ReviewedAt) {
			reviewed = false
		}
	}
	verdict := "pending"
	if reviewed {
		switch group.ReviewVerdict {
		case "approved", "commented":
			verdict = group.ReviewVerdict
		case "changes-requested", "changes_requested":
			verdict = "changes_requested"
		default:
			return SBNNReviewResult{}, fmt.Errorf("sbnn submission has unknown verdict %q", group.ReviewVerdict)
		}
	}
	findings := make([]string, 0)
	for _, comment := range group.Comments {
		if !comment.Resolved {
			findings = append(findings, fmt.Sprintf("%s:%d: %s", comment.Path, comment.StartLine, comment.Body))
		}
	}
	return SBNNReviewResult{
		CommandLine: c.Bin + " --status --json",
		Summary:     fmt.Sprintf("sbnn review for PR #%d: %s (%d open comments).", req.PRNumber, verdict, len(findings)),
		Findings:    findings, GroupURL: rec.groupURL, ReviewedCommit: rec.commitSHA,
		Reviewed: reviewed, Verdict: verdict, CreatedAt: time.Now().UTC(),
	}, nil
}

// StubSBNNClient provides a deterministic in-memory implementation of SBNNReviewer for unit tests.
type StubSBNNClient struct{}

// NewStubSBNNClient returns a new StubSBNNClient instance.
func NewStubSBNNClient() *StubSBNNClient {
	return &StubSBNNClient{}
}

// SubmitComment records an in-memory sbnn comment for unit testing.
func (s *StubSBNNClient) SubmitComment(_ context.Context, req SBNNCommentRequest) (SBNNCommentResult, error) {
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return SBNNCommentResult{}, errors.New("sbnn comment body is required")
	}

	location := "PR-level"
	cmdArgs := fmt.Sprintf("sbnn comment --target pr-%d", req.PRNumber)
	if strings.TrimSpace(req.FilePath) != "" {
		if req.Line > 0 {
			location = fmt.Sprintf("%s:%d", req.FilePath, req.Line)
		} else {
			location = req.FilePath
		}
		cmdArgs += " " + location
	}
	cmdArgs += fmt.Sprintf(" -m %q", body)

	commit := req.HeadSHA
	if commit == "" {
		commit = req.Branch
	}
	now := time.Now().UTC()
	return SBNNCommentResult{
		CommentID:      fmt.Sprintf("sbnn-cmt-%d-%d", req.PRNumber, now.UnixNano()%100000),
		CommandLine:    cmdArgs,
		Formatted:      fmt.Sprintf("[sbnn %s] %s", location, body),
		GroupURL:       fmt.Sprintf("http://127.0.0.1:0/?group=pr-%d", req.PRNumber),
		ReviewedCommit: commit,
		CreatedAt:      now,
	}, nil
}

// RunReview returns deterministic review findings for unit testing.
func (s *StubSBNNClient) RunReview(_ context.Context, req SBNNReviewRequest) (SBNNReviewResult, error) {
	focus := strings.TrimSpace(req.Focus)
	cmdArgs := fmt.Sprintf("sbnn comments --target pr-%d --format json", req.PRNumber)
	findings := []string{
		fmt.Sprintf("Checked diff between %s and %s in worktree %s.", req.BaseBranch, req.Branch, req.Worktree),
	}
	if focus != "" {
		findings = append(findings, fmt.Sprintf("Focus area: %s.", focus))
	}
	commit := req.HeadSHA
	if commit == "" {
		commit = req.Branch
	}
	summary := fmt.Sprintf("sbnn review completed for PR #%d (%d finding(s)): %s", req.PRNumber, len(findings), strings.Join(findings, " "))
	return SBNNReviewResult{
		CommandLine:    cmdArgs,
		Summary:        summary,
		Findings:       findings,
		GroupURL:       fmt.Sprintf("http://127.0.0.1:0/?group=pr-%d", req.PRNumber),
		ReviewedCommit: commit,
		Reviewed:       true,
		Verdict:        "changes_requested",
		CreatedAt:      time.Now().UTC(),
	}, nil
}
