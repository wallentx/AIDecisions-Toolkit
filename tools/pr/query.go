package pr

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String, $pageSize: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
		  reviewThreads(first: $pageSize, after: $cursor) {
			totalCount
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          originalLine
			comments(first: $pageSize) {
			  totalCount
            nodes {
              id
			  fullDatabaseId
              url
              author { login }
              authorAssociation
              body
              createdAt
              updatedAt
              path
              line
              originalLine
              diffHunk
              outdated
              commit { oid }
              originalCommit { oid }
            }
            pageInfo { hasNextPage endCursor }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const reviewThreadCommentsQuery = `query($threadID: ID!, $cursor: String, $pageSize: Int!) {
  node(id: $threadID) {
    ... on PullRequestReviewThread {
		comments(first: $pageSize, after: $cursor) {
		  totalCount
        nodes {
          id
		  fullDatabaseId
          url
          author { login }
          authorAssociation
          body
          createdAt
          updatedAt
          path
          line
          originalLine
          diffHunk
          outdated
          commit { oid }
          originalCommit { oid }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`
