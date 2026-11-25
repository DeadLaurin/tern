# Go Concurrency & You

Goroutines are **cheap** and *fun*. Use `go f()` to start one today.

Read the [official docs](https://go.dev/doc/) and the [channels guide](/articles/channels).

## Why channels?

- Communicate by **sharing memory**
- Wait for work to finish
  - With `sync.WaitGroup`
  - Or with a done channel

### Steps

1. Start the worker
2. Send jobs
3. Close the channel

> Don’t communicate by sharing memory; share memory by communicating.

```go
func main() {
    ch := make(chan int)
    go func() { ch <- 1 }()
    fmt.Println(<-ch)
}
```

---

Entities: © 2024 — "quoted" 'single' &amp; done.
