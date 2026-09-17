package slack

import "time"

// flowWait is the budget a test gives the adapter to reach a dispatch or a
// post. The work is spread over goroutines and a fake HTTP server, so on a
// loaded runner (CI, or the package running more than once in one process)
// a short budget expires before slow scheduling, not because the adapter is
// wrong. Waits that assert a timeout behaviour set their own budget.
const flowWait = 10 * time.Second
