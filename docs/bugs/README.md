# Bug reports

One file per bug, named for the symptom rather than the cause — the symptom is
what the next person searches for, and the cause is often wrong on first
writing.

Every report opens with a **Status** line. That line is the contract:

- **open** — still true of the server. These live here, in `docs/bugs/`, and
  are worth re-reading when picking up work.
- **fixed in `<version>`** — done, and moved to [`fixed/`](fixed/), with a
  *What was done* section at the end saying what was changed, what was
  deliberately left alone, and what the tests actually assert.

The split exists so that "check the bugs" means reading only what is still
true. A fixed report is not deleted, because the reasoning in it is the record
of why the code looks the way it does, and several of them are cited from
comments in the code itself — check for references before renaming or removing
one.

A report that turns out not to be a Silo defect still belongs in `fixed/` once
it is resolved elsewhere. `adding-a-number-to-a-token-response-breaks-clients.md`
is the example: the bug was a client's, but the trigger would have been a change
here, and the trap it describes is waiting for the next field added to any
response an existing client already parses.
