# One stage per binary. The binaries are cross-compiled outside Docker into
# linux/<arch>/ in the build context (hack/images.sh for kind, make dist for a
# release), so a stage only copies one in, and any platform builds anywhere.

FROM scratch AS waterwheel-controller
ARG TARGETARCH
COPY linux/${TARGETARCH}/waterwheel-controller /usr/local/bin/waterwheel-controller
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/waterwheel-controller"]

# Runs as root: it creates the socket on the node and reads other processes'
# /proc entries. The chart drops every capability except SYS_PTRACE.
FROM scratch AS waterwheel-agent
ARG TARGETARCH
COPY linux/${TARGETARCH}/waterwheel-agent /usr/local/bin/waterwheel-agent
ENTRYPOINT ["/usr/local/bin/waterwheel-agent"]

FROM scratch AS waterwheel-proxy
ARG TARGETARCH
COPY linux/${TARGETARCH}/waterwheel-proxy /usr/local/bin/waterwheel-proxy
ENV PATH=/usr/local/bin
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/waterwheel-proxy"]
