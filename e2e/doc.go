// Package e2e runs the camback binary against a real OpenSSH server in
// Docker. The tests only build with the e2e tag:
//
//	go test -tags e2e ./e2e/...
//
// They need Docker and exiftool, and run on Linux only. The server image
// (e2e/sshd) chroots SFTP like Synology DSM, so shell_path_map is needed for
// sha256sum, and its OpenSSH sftp-server refuses to rename onto an existing
// file, unlike the in-process test servers used by the unit tests.
package e2e
