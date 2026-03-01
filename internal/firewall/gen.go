package firewall

//go:generate go tool bpf2go -tags linux -no-strip -cc clang -cflags "-O2 -g -Wall -Werror -I/usr/include/x86_64-linux-gnu -I/usr/include/aarch64-linux-gnu" firewall firewall.c
