module cosmossdk.io/core

go 1.23.2

require github.com/cosmos/gogoproto v1.7.2

require (
	github.com/google/go-cmp v0.7.0 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)

// Version tagged too early and incompatible with v0.50 (latest at the time of tagging)
retract v0.12.0
