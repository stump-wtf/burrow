module gitea.stump.rocks/stump.wtf/burrow

go 1.26.0

// The toolchain line, not the go line, is the release CI tests with:
// setup-go installs what the go line names, and the go command then switches
// to this one (GOTOOLCHAIN=auto). Keep it on the same minor as the golang
// image in the Dockerfile, so the release that tests the code is the one
// that builds the image.
toolchain go1.27.1

require (
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/jackc/pgx/v5 v5.11.0
	golang.org/x/oauth2 v0.37.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)
