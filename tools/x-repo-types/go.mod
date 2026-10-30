module github.com/algorand/go-algorand/tools/x-repo-types

go 1.25.0

toolchain go1.25.3

replace github.com/algorand/go-algorand => ../..

// dt-algorand's types are compared with go-dtalgorand-sdk, the SDK built for
// it, which exports them from dt-algorand. Until its first release tag this
// pins a commit of its dtalgorand branch; move it when the SDK changes.
// scripts/test_x_repo_types.sh compares with a local checkout instead.
replace github.com/algorand/go-algorand-sdk/v2 => github.com/d13co/go-dtalgorand-sdk/v2 v2.0.0-20261030220607-c86c54572ed7

require (
	github.com/algorand/go-algorand v0.0.0
	github.com/spf13/cobra v1.8.1
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/spf13/pflag v1.0.6 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
