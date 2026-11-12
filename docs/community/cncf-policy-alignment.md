# CNCF policy alignment

This is early preparation to align project documentation and community policies with CNCF guidance. No submission date or maturity level has been selected. These notes are not an application, a claim of CNCF membership, or a completed compliance assessment.

New governance and response procedures are proposals for maintainer review. Documenting a policy does not establish that it has been adopted or exercised.

## Reference material

- [CNCF project templates](https://github.com/cncf/project-template/tree/e66ba44f724d00f66c84932593c5a229fee3564b), including the maintainer roster and maintainer-led governance examples.
- [CNCF maintainer guidance](https://contribute.cncf.io/maintainers/).
- [CNCF Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md), already adopted by the project.

Templates provide useful starting points, not a requirement to use a particular voting system. The proposed voting thresholds and lifecycle mechanics are local design choices. This work does not introduce a CLA, DCO requirement, license change, legal agreement, or duplicate machine-readable roster.

## Preparation in this PR

| Area                            | Documentation                                                                                                                                         | Remaining policy decisions                                                                                                                                  |
|---------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Maintainer information          | [Roster](../../MAINTAINERS.md) matching [OWNERS](../../OWNERS), with names, public contacts, affiliations, responsibilities, and emeritus recognition | Adopt the proposed 30-day affiliation-update policy and keep role records accurate. Verify actual permissions separately.                                   |
| Governance and role progression | Proposed [governance](../../GOVERNANCE.md) and [contributor ladder](../../CONTRIBUTOR_LADDER.md)                                                      | Review and adopt decision, appointment, and offboarding rules. Resolve the voting-quorum recovery design before adoption.                                   |
| Vendor neutrality               | Proposed merit-based leadership and decision rules in governance                                                                                      | Review actual project direction, branding, metadata, resources, and account control; policy wording alone is not evidence of neutral operation.             |
| Conduct                         | Discoverable [Code of Conduct](../../CODE_OF_CONDUCT.md) and cross-links                                                                              | Establish confidential project reporting contacts, an independent alternative, and confirmed handling responsibilities.                                     |
| Security                        | Existing private reporting address and proposed [response process](../../SECURITY.md)                                                                 | Confirm willing responders, mailbox access, private escalation, and independent handling of conflicts before adopting collective maintainer responsibility. |
| Contributor entry points        | [Contribution guide](../../CONTRIBUTING.md), issue/PR templates, and community links                                                                  | Keep instructions and channels current and make progression accessible to contributors.                                                                     |

Keep the PR draft while the proposed policies and explicitly unresolved response and governance decisions are under review. Existing documentation and recorded roles do not by themselves prove active capacity, permission settings, or access to private reporting channels.

## Existing maintainer history

Repository history provides examples of ownership changes:

- [Add Thameez as an approver](https://github.com/cloudpilot-ai/karpenter-provider-gcp/commit/c1212747).
- [Add Dmitry as a maintainer](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/254).
- [Add Joe as approver and reviewer](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/688).
- Current [OWNERS](../../OWNERS) records `patrostkowski` as an emeritus approver.

These are recorded changes, not evidence of historical use of the newly proposed nomination, voting, or offboarding rules.

## Communication inventory to verify

Existing repository documents advertise the following channels. Confirm which remain active and whether any other public or private channels need documenting.

| Channel                                                                                                                                                            | Documented purpose / source                                                                            |
|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------|
| [GitHub Issues](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues) and [pull requests](https://github.com/cloudpilot-ai/karpenter-provider-gcp/pulls) | Public bugs, requests, reviews, and design decisions; README and contribution guide                    |
| [Kubernetes Slack channel](https://kubernetes.slack.com/archives/C0B20K4KWP8)                                                                                      | Community chat; README                                                                                 |
| [CloudPilot AI community Slack channel](https://cloudpilotaicommunity.slack.com/archives/C093V65481H)                                                              | Community chat; docs index                                                                             |
| [Discord](https://discord.gg/WxFWc87QWr)                                                                                                                           | Community chat; docs index                                                                             |
| [Security email](mailto:security@cloudpilot.ai)                                                                                                                    | Private vulnerability reports; security policy. Access and response ownership still need confirmation. |

No new meeting schedule, private maintainer list, or confidential conduct channel is established here. Do not disclose confidential incidents to fill out a public channel inventory.

## If a future application is considered

Choose the timing and maturity level first, then assess the [current CNCF process](https://github.com/cncf/toc/tree/main/process) and the applicable criteria. Depending on that process, further work may include technical, governance, or security assessments; security practices/badges; adoption evidence; community activity and planning records; and legal, branding, or asset-control review. These are not asserted to be universal requirements or commitments made by this PR.

Existing [project documentation](../index.md), [release process](../../RELEASE.md), [adopter list](../../ADOPTERS.md), and [license](../../LICENSE.txt) can support that future evaluation, but do not substitute for verification. Confirm the full project/subproject scope and any related-project disclosures when relevant.

This work does not submit an application, select an admission level, contact adopters or CNCF reviewers, register a badge, change account settings, sign agreements, transfer assets, or modify infrastructure.
