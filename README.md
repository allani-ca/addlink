# addlink

Version 0.1. Unsupported and provided as-is.

Adds a profile link with a custom icon to an AT Protocol account. Requires an app password and a PNG, JPEG, or WebP icon no larger than 100,000 bytes.

```sh
./addlink <handle> <app-password> <https-url> <title> <image-file>
```

On Windows, use `addlink.exe` in place of `./addlink`. Quote arguments containing spaces. Command-line passwords can be exposed in shell history and process listings; use a dedicated app password and revoke it when finished.

Set `ATPROTO_PDS` to override PDS discovery or `ATPROTO_RESOLVER` to override the handle resolver (default: `https://bsky.social`).

Windows and Linux amd64 binaries are available in GitHub Releases. Push a version tag such as `v0.1` to build and publish a release. To build locally with Go:

```sh
go build addlink.go
```

Licensed under either the [MIT license](LICENSE-MIT.txt) or the [Apache License 2.0](LICENSE-APACHE.txt), at your option, matching the [Bluesky PDS licensing terms](https://github.com/bluesky-social/pds/blob/main/LICENSE.txt).