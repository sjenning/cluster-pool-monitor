FROM fedora:34

COPY cluster-pool-monitor /usr/local/bin

ENTRYPOINT /usr/local/bin/cluster-pool-monitor