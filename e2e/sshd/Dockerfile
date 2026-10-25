# OpenSSH server for the camback E2E tests (go test -tags e2e ./e2e/...).
#
# Like Synology DSM, SFTP is chrooted to /volume1 (so the client sees
# /photo) while commands run over SSH see the real file system
# (/volume1/photo), so sha256sum needs shell_path_map. OpenSSH's own
# ChrootDirectory would also chroot the shell, so the sftp subsystem chroots
# itself through sudo instead.
FROM ubuntu:24.04

# The test user's uid, so that files it writes to the bind-mounted
# /volume1/photo belong to the user running the tests.
ARG NAS_UID=1000

RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server sudo \
 && rm -rf /var/lib/apt/lists/*

RUN (userdel -r ubuntu 2>/dev/null || true) \
 && useradd --create-home --uid "$NAS_UID" --shell /bin/bash nas \
 && install -d -m 700 -o nas -g nas /home/nas/.ssh

# The chroot: sftp-server with the libraries it links, the passwd/group
# entries it looks up, and /dev/null, which it opens at start.
RUN mkdir -p /volume1/photo /volume1/etc /volume1/dev \
 && mknod -m 666 /volume1/dev/null c 1 3 \
 && chown nas:nas /volume1/photo \
 && cp --parents /usr/lib/openssh/sftp-server \
      $(ldd /usr/lib/openssh/sftp-server | grep -o '/[^ ]*') /volume1/ \
 && grep -E '^(root|nas):' /etc/passwd > /volume1/etc/passwd \
 && grep -E '^(root|nas):' /etc/group > /volume1/etc/group

COPY sftp-chroot /usr/local/sbin/sftp-chroot
COPY camback.conf /etc/ssh/sshd_config.d/camback.conf
RUN chmod 755 /usr/local/sbin/sftp-chroot \
 && echo 'nas ALL=(root) NOPASSWD: /usr/local/sbin/sftp-chroot' > /etc/sudoers.d/sftp-chroot \
 && chmod 440 /etc/sudoers.d/sftp-chroot \
 && sed -i '/^Subsystem/d' /etc/ssh/sshd_config \
 && mkdir -p /run/sshd \
 && ssh-keygen -A

EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
