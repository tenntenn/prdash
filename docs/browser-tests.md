# Browser regression tests

The draft-preservation tests run the real dashboard JavaScript in Chromium with
controlled HTTP and SSE responses. They cover input and selection during updates,
failed submissions, successful submission isolation, edits made while sending,
filter changes, and refresh failures after an accepted submission.

Install the public Playwright test dependency under `tools/browser` with
`npm ci --prefix tools/browser`, then install Chromium with
`node tools/browser/node_modules/playwright/cli.js install chromium` and run
`node --test tools/browser/drafts.test.mjs`. Set
`PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` to an existing Chrome/Chromium executable to
use that instead. CI installs Chromium and runs these tests automatically.

Unsent form drafts are kept in memory per PR, session, or comment form. Server
updates and reconnects preserve them; a full page reload does not. A successful
submission clears only the submitted form, and keeps any edits made while that
request was in flight. Failed submissions retain the draft and show an inline
error.
