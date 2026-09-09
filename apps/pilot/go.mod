module github.com/vivek7405/pilots/cli

go 1.26

replace github.com/vivek7405/pilots/sdks/go => ../../sdks/go

require (
	github.com/spf13/cobra v1.10.2
	github.com/vivek7405/pilots/sdks/go v0.0.0-00010101000000-000000000000
)

require (
	github.com/coder/websocket v1.8.15 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
)
