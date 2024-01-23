.PHONY: all

all: proto/mayakashi.pb.go

clean:
	rm proto/*.pb.go

proto/%.pb.go: proto/%.proto
	protoc -I proto/ --go_out=. --go_opt=Mmayakashi.proto=proto/ $<