## sbnn review lifecycle

A review request prepares the PR diff from its assigned worktree and base branch,
then delivers it to a repository-scoped sbnn group. A known head commit is pinned
when reading the diff. If a local unpinned diff is unavailable, the GitHub fallback
checks head and base commits before and after fetching the diff.

Delivery is not approval. prdash reads the live sbnn group through its local HTTP
API and records its URL, full reviewed commit, submission state, verdict, and
unresolved comments in `sbnn_reviews`. A newly delivered diff remains pending
until the reviewer submits. An approval with comments remains an approval;
comments are retained for follow-up. Failed commands, malformed responses, and
unavailable groups return errors instead of a successful empty review.

Before sending, prdash compares the live group's latest diff and commit labels.
Repeated requests, including requests after restarting prdash, reuse the matching
round. Requests in one prdash process are serialized across clients. New rounds
append to the existing group without clearing outstanding comments. Separate
prdash processes should not dispatch reviews for the same PR concurrently because
sbnn does not offer an atomic check-and-add operation. Old `pr-N` groups from
previous versions are left untouched; repository-scoped groups use a `prdash-`
prefix so PR numbers from different repositories cannot collide.

If the PR target changes while a result is being retrieved, the result remains in
history with `stale: true` but does not change the current PR's status. sbnn must
run on a loopback HTTP address; redirecting or remote endpoints are rejected.
