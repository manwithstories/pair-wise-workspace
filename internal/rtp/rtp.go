// Package rtp reorders RTP packets: duplicates are counted once,
// out-of-window late packets are isolated, gaps are marked per the
// recovery policy.
package rtp
