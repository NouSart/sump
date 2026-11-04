module github.com/NouSart/sump/timer

go 1.26

require (
	github.com/NouSart/sump/protocol v0.0.0
	github.com/NouSart/sump/service v0.0.0
	github.com/gorilla/websocket v1.5.3
)

require (
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
)

replace github.com/NouSart/sump/protocol => ../protocol

replace github.com/NouSart/sump/service => ../service
