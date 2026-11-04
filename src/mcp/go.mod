module github.com/NouSart/sump/mcp

go 1.26

require (
	github.com/NouSart/sump/protocol v0.0.0
	github.com/NouSart/sump/service v0.0.0-00010101000000-000000000000
	github.com/google/jsonschema-go v0.4.3
	github.com/gorilla/websocket v1.5.3
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/vmihailenco/msgpack/v5 v5.4.1
)

require (
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/NouSart/sump/protocol => ../protocol

replace github.com/NouSart/sump/service => ../service
