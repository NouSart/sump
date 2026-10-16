module github.com/QAQ-awa-QAQ/sump/approval-integration

go 1.26

require (
	github.com/QAQ-awa-QAQ/sump/approval v0.0.0
	github.com/QAQ-awa-QAQ/sump/protocol v0.0.0
	github.com/QAQ-awa-QAQ/sump/reasoner v0.0.0
	github.com/QAQ-awa-QAQ/sump/service v0.0.0
	github.com/QAQ-awa-QAQ/sump/settings-center v0.0.0
	github.com/QAQ-awa-QAQ/sump/shell v0.0.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
)

replace github.com/QAQ-awa-QAQ/sump/approval => ../approval

replace github.com/QAQ-awa-QAQ/sump/protocol => ../protocol

replace github.com/QAQ-awa-QAQ/sump/service => ../service

replace github.com/QAQ-awa-QAQ/sump/settings-center => ../settings-center

replace github.com/QAQ-awa-QAQ/sump/shell => ../shell

replace github.com/QAQ-awa-QAQ/sump/reasoner => ../reasoner
