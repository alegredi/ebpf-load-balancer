package bpf

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target bpf -cflags "-Wno-missing-declarations -I../../bpf -I/usr/include" -type backend_info -type service_config -type stats_record xdpLb ../../bpf/xdp_lb.bpf.c
