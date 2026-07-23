# SPDX-License-Identifier: AGPL-3.0-only

# Testdata for DNSRecordSet ActivityPolicy (issue #62 / #36).
# Fixtures are exercised by Go tests in internal/activitypolicy/.
#
# Covered behaviours:
# - Create with display-name → hostname in summary (create-txt)
# - Portal PATCH without requestObject.spec.recordType → update-a-aaaa (#36)
# - Delete with annotations → "deleted" + hostname
# - Metadata-only patch → no update rule match
