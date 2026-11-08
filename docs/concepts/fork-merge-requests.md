# 🍴 Fork merge requests

The checkout first tries to fetch the base and head commits by SHA. When the server refuses that (this is common for merge requests from forks), it fetches `refs/merge-requests/<iid>/head` (GitHub: `refs/pull/<number>/head`) instead, then deepens the target branch (starting at 50 commits and doubling, up to 3200) until the merge base is there. If the merge base is still missing at that depth, the checkout fails and the merge request gets an error comment.
