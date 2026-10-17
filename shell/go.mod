module github.com/QAQ-awa-QAQ/sump/shell

go 1.26

require (
	github.com/QAQ-awa-QAQ/sump/protocol v0.0.0
	github.com/QAQ-awa-QAQ/sump/service v0.0.0
	github.com/gorilla/websocket v1.5.3
	golang.org/x/text v0.43.0
)

require (
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
)

replace github.com/QAQ-awa-QAQ/sump/protocol => ../protocol

replace github.com/QAQ-awa-QAQ/sump/service => ../service
