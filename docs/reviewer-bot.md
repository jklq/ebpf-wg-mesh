# Automated pull request review

This repository is reviewed by a self-hosted `reviewd` instance.

Opening, updating, or reopening a non-draft pull request, or marking it ready for
review, starts an automated review of the current head. A project owner, member, or
collaborator can request a fresh review of the current head by commenting exactly:

```text
@reviewd review
```

The review is posted as a pull request review with a merge-confidence score, ordered
findings, and a summary. Draft and closed pull requests are skipped.
