package tf

import "testing"

func TestFinishedLines(t *testing.T) {
	n := 0
	f := &finishedLines{step: func() { n++ }}
	out := "aws_vpc.this: Creating...\n" +
		"aws_vpc.this: Creation complete after 2s [id=vpc-1]\n" +
		"aws_subnet.a: Still creating... [10s elapsed]\n" +
		"aws_subnet.a: Modifications complete after 1s\n" +
		"data.aws_ami.x: Read complete after 0s\n" +
		"aws_eip.nat: Destruction complete after 3s\n" +
		"aws_route.r: Creation comp"
	// Split mid-line, as a pipe may deliver it.
	f.Write([]byte(out[:40]))
	f.Write([]byte(out[40:]))
	if n != 3 {
		t.Fatalf("got %d finished, want 3", n)
	}
	f.Write([]byte("lete after 1s\n"))
	if n != 4 {
		t.Fatalf("got %d finished, want 4", n)
	}
}
