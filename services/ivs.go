package services

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ivs"
	"github.com/aws/aws-sdk-go-v2/service/ivs/types"
)

type StreamCredentials struct {
	ChannelARN     string
	IngestURL      string
	StreamKey      string
	PlaybackURL    string
}

type IVSService struct {
	client  *ivs.Client
	Enabled bool
}

func NewIVSService(accessKeyID, secretKey, region string) *IVSService {
	if accessKeyID == "" || secretKey == "" {
		return &IVSService{Enabled: false}
	}
	cfg := aws.Config{
		Region: region,
		Credentials: credentials.NewStaticCredentialsProvider(accessKeyID, secretKey, ""),
	}
	return &IVSService{
		client:  ivs.NewFromConfig(cfg),
		Enabled: true,
	}
}

func (s *IVSService) ProvisionChannel(ctx context.Context, eventTitle string) (*StreamCredentials, error) {
	if !s.Enabled {
		return nil, fmt.Errorf("IVS not configured — set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}

	out, err := s.client.CreateChannel(ctx, &ivs.CreateChannelInput{
		Name:        aws.String(eventTitle),
		LatencyMode: types.ChannelLatencyModeLowLatency,
		Type:        types.ChannelTypeStandardChannelType,
	})
	if err != nil {
		return nil, fmt.Errorf("IVS CreateChannel: %w", err)
	}

	return &StreamCredentials{
		ChannelARN:  aws.ToString(out.Channel.Arn),
		IngestURL:   aws.ToString(out.Channel.IngestEndpoint),
		StreamKey:   aws.ToString(out.StreamKey.Value),
		PlaybackURL: aws.ToString(out.Channel.PlaybackUrl),
	}, nil
}

func (s *IVSService) DeleteChannel(ctx context.Context, channelARN string) error {
	if !s.Enabled {
		return nil
	}
	_, err := s.client.DeleteChannel(ctx, &ivs.DeleteChannelInput{
		Arn: aws.String(channelARN),
	})
	return err
}
