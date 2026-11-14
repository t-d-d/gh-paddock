# The region
Relevance: Where the stacks live: infra/**, .github/workflows/deploy.yml, .github/actions/**
Decision: The CI account is in us-east-1, and no setting moves the stacks elsewhere.

Why: Settled with the owner in PRD #1. ECR Public issues its tokens there
alone, Fargate Spot is widely offered there, and one fixed region keeps the
README's setup and the stacks' ARNs simple.

## Rules

- The main stack's rule refuses any region but us-east-1.
- Every AWS step of the deploy names us-east-1. No repository variable sets
  the region.

## Examples

- `fitness: the stack runs in us-east-1 alone (region.md)` in
  test/stack.test.ts, and `fitness: the deploy runs in us-east-1 alone (region.md)`
  in test/deploy-workflow.test.ts.

## Do not

- Do not bring back a region variable: a stack applied in a second region
  would start a second listener against the same scale sets.

History: Settled with the owner in PRD #1, and written by #16.
