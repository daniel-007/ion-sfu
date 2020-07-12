// Package sub-to-browser contains an example of subscribing to a stream from
// an ion-sfu instance in the browser.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	sfu "github.com/pion/ion-sfu/cmd/server/grpc/proto"
	"github.com/pion/ion-sfu/examples/internal/signal"
	"github.com/pion/webrtc/v2"
	"google.golang.org/grpc"
)

const (
	address = "localhost:50051"
)

func main() {
	// Set up a connection to the server.
	conn, err := grpc.Dial(address, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()
	c := sfu.NewSFUClient(conn)

	subOffer := webrtc.SessionDescription{}
	signal.Decode(signal.MustReadStdin(), &subOffer)

	mid := os.Args[1]

	ctx := context.Background()
	stream, err := c.Subscribe(ctx)

	if err != nil {
		log.Panicf("Error subscribing: %v", err)
	}

	err = stream.Send(&sfu.SubscribeRequest{Mid: mid, Payload: &sfu.SubscribeRequest_Connect{
		Connect: &sfu.Connect{
			Description: &sfu.SessionDescription{
				Type: subOffer.Type.String(),
				Sdp:  []byte(subOffer.SDP),
			},
		},
	}})

	if err != nil {
		log.Fatalf("Error sending subscribe request: %v", err)
	}

	for {
		res, err := stream.Recv()
		if err == io.EOF {
			// WebRTC Transport closed
			fmt.Println("WebRTC Transport Closed")
			return
		}

		if err != nil {
			log.Fatalf("Error receving publish response: %v", err)
		}

		switch payload := res.Payload.(type) {
		case *sfu.SubscribeReply_Connect:
			// Output the mid and answer in base64 so we can paste it in browser
			fmt.Printf("\nsub mid: %s", res.Mid)
			fmt.Printf("\nsub answer: %s", signal.Encode(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  string(payload.Connect.Description.Sdp),
			}))
		}
	}
}
