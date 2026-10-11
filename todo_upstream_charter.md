# Charter upstream gaps

## Bounded execution-context actions

`evidence.Provider.ActionsIn` returns every action in an execution context; `BaseProvider` scans the full corpus. Add a limit or cursor to the neutral API so Scout run traces can bound the provider read, not only their returned events. Until then, large Charter corpora should use an indexed provider and a request deadline.
