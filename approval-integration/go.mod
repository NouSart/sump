module github.com/NouSart/sump/approval-integration

go 1.26.0

require (
	github.com/NouSart/sump/approval v0.0.0
	github.com/NouSart/sump/protocol v0.0.0
	github.com/NouSart/sump/reasoner v0.0.0
	github.com/NouSart/sump/service v0.0.0
	github.com/NouSart/sump/settings-center v0.0.0
	github.com/NouSart/sump/shell v0.0.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/NouSart/sump/approval => ../approval

replace github.com/NouSart/sump/protocol => ../protocol

replace github.com/NouSart/sump/service => ../service

replace github.com/NouSart/sump/settings-center => ../settings-center

replace github.com/NouSart/sump/shell => ../shell

replace github.com/NouSart/sump/reasoner => ../reasoner
