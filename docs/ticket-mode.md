# Ticket mode: `clawk work`

CWD mode (bare `clawk`) and ticket mode (`clawk work`) are one lifecycle with
two creation recipes: create a sandbox if it doesn't exist, then attach the
agent. Once a sandbox exists — whichever mode made it — **`clawk attach
<name>`** gets you back in from any directory, booting the VM first if it's
stopped. Attach never creates and never re-reads a template; it only resumes.

## CWD mode — `clawk`

Zero arguments. Creates (or resumes) a sandbox keyed on your current
directory, mounts it as-is, launches the default runner. No git involvement,
no template, no ticket. Persistent until you `destroy` it.

```sh
clawk                       # cwd-vm + claude
clawk run shell             # cwd-vm + plain shell
clawk run codex             # cwd-vm + codex
clawk down                  # stop the VM (disk persists)
clawk destroy               # nuke the VM (the agent's host-side state persists)
```

Sandbox names are derived from `filepath.Base(cwd)`, with a sha256 suffix on
collision (e.g. two projects called `clawk` resolve to distinct names).

## Ticket mode — `clawk work`

You have a ticket. It might touch 1–N repos. `clawk work <ticket>` reads
a template, snapshots its configuration into a new sandbox, materializes
one git worktree per repo on a fresh branch named after the ticket, and
attaches the default runner.

The template is a `clawk.mod`'s `sandbox` block — multi-repo when it has
`includes ( … )`, single-repo otherwise. It is read **once at create
time** and snapshotted into the sandbox record — editing the template
after creation does not change live sandboxes; the sandbox is
self-contained from creation onward.

```sh
cd ~/code/my-workspace          # contains a clawk.mod
clawk work INFRA-123         # snapshots template, creates worktrees, attaches claude
clawk status INFRA-123       # branch + PR overview
clawk pr INFRA-123           # open cross-linked PRs for each repo
clawk worktree rebase INFRA-123 some-repo
                                # rebase open follow-ups onto origin/<default>
```

When a branch's PR is already merged and you need a follow-up worktree in
the same repo, the next `worktree add` produces `INFRA-123-2`, then `-3`,
and so on — recognizable, no collision with the original PR's branch.

For non-default runners, prepare the sandbox without booting and attach
the runner you want:

```sh
clawk work INFRA-123 --bare
clawk run codex INFRA-123
```

## A ticket, end to end

This is the ticket flow (`clawk work`) over its whole life; single-repo work
(`clawk work` from inside one repo) is identical with a single worktree.

**Start.** One command materializes the sandbox and attaches the agent:

```sh
cd ~/code/my-workspace
clawk work INFRA-123          # a worktree per repo on branch INFRA-123, claude attached
```

**Detach and reattach.** Disconnecting ends the agent process but leaves the
VM running; reattach whenever, from any directory — claude picks up from its
own on-disk state, and a stopped VM is booted automatically first:

```sh
clawk attach INFRA-123                   # reattach the default agent
clawk attach INFRA-123 -- --resume       # ...resuming the previous conversation
clawk run shell INFRA-123                # a shell in the same VM
clawk run codex INFRA-123                # or a different runner
```

**Check state.** `clawk status` lists each worktree's branch and its PR status
— `pending`, `active` (PR open), or `merged` — read from `gh`:

```sh
clawk status INFRA-123
```

**Open PRs.** When the work is ready, push every branch and open one PR per
repo (needs the `gh` CLI). Re-running is safe: an existing PR is found and
reused, and a repo with no changes versus its base is skipped. PR descriptions
are left empty for you or the agent to fill in:

```sh
clawk pr INFRA-123
clawk pr INFRA-123 --draft        # open them as drafts
clawk pr INFRA-123 --base develop
```

**Follow up after a merge.** When a repo's PR merges and the ticket needs more
work in that same repo, add a fresh worktree. clawk bumps the branch to
`INFRA-123-2` (then `-3`, …) so it never collides with the merged PR's branch,
and starts it from the repo's default branch:

```sh
clawk worktree add INFRA-123 some-repo      # new branch INFRA-123-2
clawk worktree rebase INFRA-123 some-repo   # rebase it onto the just-merged code
clawk pr INFRA-123                          # opens the follow-up PR
```

Repos move independently — one can be on its third follow-up while another is
still awaiting its first review.

**Finish.** Stop the sandbox to pick it up later, or remove it when the ticket
is done. `destroy` deletes the VM, its worktrees, and its network rules;
conversation history and memory stay on the host (see
[What survives what](../README.md#what-survives-what)):

```sh
clawk down INFRA-123        # stop; resume later with clawk attach
clawk destroy INFRA-123     # remove (warns on dirty/unpushed worktrees; -f forces)
```
