(() => {
  function matches(pr, session, worktree, query) {
    const terms = String(query || "").trim().toLocaleLowerCase().split(/\s+/).filter(Boolean);
    const fields = [pr.number, `#${pr.number}`, pr.title, pr.branch, pr.worktree_id,
      worktree?.path, worktree?.branch, pr.session_id, session?.name]
      .filter((value) => value != null).map((value) => String(value).toLocaleLowerCase());
    return terms.every((term) => fields.some((field) => field.includes(term)));
  }
  window.prdashSearch = Object.freeze({ matches });
})();
