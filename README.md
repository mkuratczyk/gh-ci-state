# gh-ci-state
Reports CI workflow run history with a compact trend and latest-run details

## Example

```
gh-ci-state -R rabbitmq/omq -workflow ci.yaml
✅✅✅✅✅ 2/2 passed (3h ago)
```

This immediatelly tells us that the last 5 runs of the ci.yaml workflow succeeded
