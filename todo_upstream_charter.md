# Charter upstream gaps

## Latest bounded actions

`evidence.Queries.ActionsIn` returns the first limited actions in an execution context. Add a newest-first option or cursor so Scout can reconcile the latest tool attempt without reading an unbounded history. Until then, Scout fails closed when the bounded result is full.
