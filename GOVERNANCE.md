# Project governance

## Purpose and values

Karpenter GCP Cloud Provider provides open, workload-aware node provisioning for GKE Standard using Karpenter's Kubernetes APIs. Project goals and use cases are described in the [README](README.md) and [documentation](docs/index.md).

Project decisions are made in the interests of users and contributors, not a sponsor's product or commercial interests. Contributions, leadership, and roadmap priorities are evaluated on merit. Employment, sponsorship, or infrastructure funding gives no organization ownership of a role, veto, or preferred treatment. Maintainers disclose relevant conflicts and recuse themselves from decisions where their personal interests prevent impartial judgment. Sharing an employer alone does not disqualify participation.

Technical and governance discussions take place in public issues and pull requests, with decisions recorded for future contributors. Security, personal information, and conduct reports are handled privately. All participants follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Roles and ownership

Anyone can contribute code, documentation, tests, reviews, or community support. Reviewers review changes in their areas of expertise. Maintainers (approvers) approve changes and collectively steward technical direction, reviews, releases, documentation, community health, and project resources. Emeritus maintainers are recognized for past contributions but have no voting rights or access entitlement.

The [maintainer roster](MAINTAINERS.md) lists project-wide maintainers; [OWNERS](OWNERS) records reviewers and approvers. Appointment and departure procedures are described [below](#joining-and-leaving-roles).

A recorded role is not proof of administrative access. Administrators grant only the access needed for an accepted role and review permissions on role changes. A delegated team requires a recorded purpose, named willing members, scope, appointment decision, and removal process. Delegates use the same nomination and removal votes as maintainers, but do not receive maintainer votes merely by joining a team. Appointments, onboarding, offboarding, and permissions are recorded publicly except for sensitive details.

## Decisions

Routine contributions use public pull requests, review by an appropriate reviewer/approver, and applicable automated checks. An author must obtain approval from another maintainer before merging their own changes. Significant technical changes follow the [design proposal process](CONTRIBUTING.md#design-proposals). An unresolved substantive objection is discussed rather than silently overridden; any maintainer can request a recorded vote.

The voting rules below are project choices, not CNCF-prescribed rules:

| Decision                                                                                                                                     | Required affirmative votes                              |
|----------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------|
| Contested technical decisions, reviewer/maintainer appointments, delegated-team appointments, requests to CNCF, or project resource requests | More than half of eligible maintainers                  |
| Governance or project-goal changes, or removal from a reviewer/maintainer/delegated role                                                     | At least two-thirds of eligible maintainers, rounded up |

Eligible voters are the active maintainers listed in `MAINTAINERS.md`, excluding the subject of a removal or appointment and anyone recused for a conflict. Emeritus maintainers do not vote. The denominator includes eligible maintainers who abstain or do not respond; affirmative votes must meet the threshold and include at least two people. If fewer than two are eligible, the decision is blocked; there is no unilateral override. Maintainers must document a narrowly bounded recovery procedure for loss of the voting quorum, including who can authorize replacement appointments. A governance amendment subject to the same quorum is not itself a recovery mechanism.

A vote is announced in a public issue or pull request, with the proposal, eligible voters, and a deadline at least seven calendar days later. A tie or insufficient support leaves the status quo in place; proponents can revise and reopen discussion. Record the result and rationale. Confidential personnel, security, or conduct matters exclude implicated people from private deliberation; publish a redacted outcome when safe without exposing reporters or sensitive data.

Any contributor may propose project direction or resource requests. Maintainers apply the voting rules above before committing the project to CNCF requests, new goals, or governance changes. Legal agreements still require authorization from the actual rights holders; a maintainer vote does not substitute for it.

## Joining and leaving roles

Contributors can express interest in reviewing through a public issue or pull request. An existing maintainer nominates a reviewer or maintainer, with examples of sustained contributions, useful reviews, collaboration, and familiarity with the relevant code, documentation, and project practices. There is no employment, sponsorship, fixed contribution count, or company-membership prerequisite. Candidates must accept the responsibilities before a vote.

After approval, a maintainer coordinates onboarding: explain review/release practices and the Code of Conduct, update `OWNERS` and the roster/role scope, and have an administrator grant and verify necessary access. Confirm private response duties and access separately; a public role does not automatically grant access to private reports or the fallback mailbox.

Anyone may resign or request emeritus status. Maintainers can propose removal for sustained inability to perform the role, misconduct, or loss of trust. Before a removal vote, offer the person an opportunity to respond privately where needed for safety. Inactivity prompts a conversation about availability, not automatic removal after an invented fixed period. Voluntary departures do not require a removal vote.

Offboarding includes updating role records and having administrators revoke unneeded repository, release, infrastructure, and private-channel access. Emeritus recognition preserves credit but carries no vote or access entitlement. Returning to an active role follows the normal nomination and onboarding process.

## Security and conduct responsibilities

The maintainers collectively form the security response team; duties, access requirements, and disclosure handling are in [SECURITY.md](SECURITY.md). Any future delegation follows the appointment and removal process above, with responsibility and access reviewed at least annually.

The project adopts the [CNCF Code of Conduct](CODE_OF_CONDUCT.md). Maintainers must establish confidential project reporting contacts and an independent alternative for reports involving those contacts. Do not treat a GitHub mention as a confidential reporting mechanism or assume CNCF committee jurisdiction.

## Communication and project structure

Public community channels are listed in the [README](README.md#community) and [documentation](docs/index.md#community). Decisions made in chat or meetings must be summarized in a public issue or pull request, except for appropriately confidential matters.
