# cubby

A local disk cache for GitHub Actions on self-hosted runners, available as a
Go library and command. Standard library only, with tenant quotas,
repository and ref isolation, and LRU eviction.

```go
import "go.toshicloud.io/cubby"
```

Embed Cubby as an `http.Handler` with a trusted `Options.Resolve`, or use
`cubby serve` for one isolated job.

- [Usage](docs/usage.md)
- [Protocol](PROTOCOL.md)
- [Compatibility](compatibility/README.md)

Experimental; live runner validation is still pending.

[Apache-2.0](LICENSE) · [NOTICE](NOTICE)
