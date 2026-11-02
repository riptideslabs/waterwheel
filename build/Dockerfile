# One stage per binary; the binaries are cross-compiled on the host
# (hack/images.sh) and copied from the build context.

FROM scratch AS waterwheel-controller
COPY waterwheel-controller /usr/local/bin/waterwheel-controller
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/waterwheel-controller"]

# Runs as root: it creates the socket on the node and reads other processes'
# /proc entries. The chart drops every capability except SYS_PTRACE.
FROM scratch AS waterwheel-agent
COPY waterwheel-agent /usr/local/bin/waterwheel-agent
ENTRYPOINT ["/usr/local/bin/waterwheel-agent"]

FROM scratch AS waterwheel-proxy
COPY waterwheel-proxy /usr/local/bin/waterwheel-proxy
ENV PATH=/usr/local/bin
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/waterwheel-proxy"]
