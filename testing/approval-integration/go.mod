module github.com/NouSart/sump/approval-integration

go 1.26.0

require (
	github.com/NouSart/sump/approval v0.0.0
	github.com/NouSart/sump/protocol v0.0.0
	github.com/NouSart/sump/reasoner v0.0.0
	github.com/NouSart/sump/service v0.0.0
	github.com/NouSart/sump/settings-center v0.0.0
	github.com/NouSart/sump/shell v0.0.0
	github.com/NouSart/sump/spv2 v0.0.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.59.0 // indirect
)

replace github.com/NouSart/sump/approval => ../../src/approval

replace github.com/NouSart/sump/protocol => ../../src/protocol

replace github.com/NouSart/sump/service => ../../src/service

replace github.com/NouSart/sump/settings-center => ../../src/settings-center

replace github.com/NouSart/sump/shell => ../../src/shell

replace github.com/NouSart/sump/reasoner => ../../src/reasoner

replace github.com/NouSart/sump/spv2 => ../../src/spv2
