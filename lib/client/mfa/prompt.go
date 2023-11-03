/*
Copyright 2023 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mfa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/client/proto"
	wancli "github.com/gravitational/teleport/lib/auth/webauthncli"
	wantypes "github.com/gravitational/teleport/lib/auth/webauthntypes"
)

// Prompt is an MFA prompt.
type Prompt interface {
	// Run prompts the user to complete an MFA authentication challenge.
	Run(ctx context.Context, chal *proto.MFAAuthenticateChallenge) (*proto.MFAAuthenticateResponse, error)
}

// PromptFunc is a function wrapper that implements the Prompt interface.
type PromptFunc func(ctx context.Context, chal *proto.MFAAuthenticateChallenge) (*proto.MFAAuthenticateResponse, error)

// Run prompts the user to complete an MFA authentication challenge.
func (f PromptFunc) Run(ctx context.Context, chal *proto.MFAAuthenticateChallenge) (*proto.MFAAuthenticateResponse, error) {
	return f(ctx, chal)
}

// PromptConfig contains common mfa prompt config options.
type PromptConfig struct {
	// ProxyAddress is the address of the authenticating proxy. required.
	ProxyAddress string
	// PromptReason is an optional message to share with the user before an MFA Prompt.
	// It is intended to provide context about why the user is being prompted where it may
	// not be obvious, such as for admin actions or per-session MFA.
	PromptReason string
	// DeviceType is an optional device description to emphasize during the prompt.
	DeviceType DeviceDescriptor
	// WebauthnLoginFunc performs client-side Webauthn login.
	WebauthnLoginFunc func(ctx context.Context, origin string, assertion *wantypes.CredentialAssertion, prompt wancli.LoginPrompt, opts *wancli.LoginOpts) (*proto.MFAAuthenticateResponse, string, error)
	// Quiet suppresses users prompts.
	Quiet bool
	// AllowStdinHijack allows stdin hijack during MFA prompts.
	// Stdin hijack provides a better login UX, but it can be difficult to reason
	// about and is often a source of bugs.
	// Do not set this options unless you deeply understand what you are doing.
	// If false then only the strongest auth method is prompted.
	AllowStdinHijack bool
	// AuthenticatorAttachment specifies the desired authenticator attachment.
	AuthenticatorAttachment wancli.AuthenticatorAttachment
	// PreferOTP favors OTP challenges, if applicable.
	// Takes precedence over AuthenticatorAttachment settings.
	PreferOTP bool
	// WebauthnSupported indicates whether Webauthn is supported.
	WebauthnSupported bool
}

// DeviceDescriptor is a descriptor for a device, such as "registered".
type DeviceDescriptor string

// DeviceDescriptorRegistered is a registered device.
const DeviceDescriptorRegistered = "registered"

// DefaultPromptConfig returns a prompt config that will induce default behavior.
func DefaultPromptConfig(proxyAddr string) *PromptConfig {
	return &PromptConfig{
		ProxyAddress:      proxyAddr,
		WebauthnLoginFunc: wancli.Login,
		WebauthnSupported: wancli.HasPlatformSupport(),
	}
}

// PromptOpt applies configuration options to a prompt.
type PromptOpt func(*PromptConfig)

// WithQuiet sets the prompt's Quiet field.
func WithQuiet() PromptOpt {
	return func(cfg *PromptConfig) {
		cfg.Quiet = true
	}
}

// WithPromptReason sets the prompt's PromptReason field.
func WithPromptReason(hint string) PromptOpt {
	return func(cfg *PromptConfig) {
		cfg.PromptReason = hint
	}
}

// WithPromptReasonAdminAction sets the prompt's PromptReason field to a standard admin action message.
func WithPromptReasonAdminAction() PromptOpt {
	const adminMFAPromptReason = "MFA is required for admin-level API request."
	return WithPromptReason(adminMFAPromptReason)
}

// WithPromptReasonSessionMFA sets the prompt's PromptReason field to a standard session mfa message.
func WithPromptReasonSessionMFA(serviceType, serviceName string) PromptOpt {
	return WithPromptReason(fmt.Sprintf("MFA is required to access %s %q", serviceType, serviceName))
}

// WithPromptDeviceType sets the prompt's DeviceType field.
func WithPromptDeviceType(deviceType DeviceDescriptor) PromptOpt {
	return func(cfg *PromptConfig) {
		cfg.DeviceType = deviceType
	}
}

// RunOpts are mfa prompt run options.
type RunOpts struct {
	PromptTOTP     bool
	PromptWebauthn bool
}

// GetRunOptions gets mfa prompt run options by cross referencing the mfa challenge with prompt configuration.
func (c PromptConfig) GetRunOptions(ctx context.Context, chal *proto.MFAAuthenticateChallenge) (RunOpts, error) {
	promptTOTP := chal.TOTP != nil
	promptWebauthn := chal.WebauthnChallenge != nil

	if !promptTOTP && !promptWebauthn {
		return RunOpts{}, trace.BadParameter("mfa challenge is empty")
	}

	// Does the current platform support hardware MFA? Adjust accordingly.
	switch {
	case !promptTOTP && !c.WebauthnSupported:
		return RunOpts{}, trace.BadParameter("hardware device MFA not supported by your platform, please register an OTP device")
	case !c.WebauthnSupported:
		// Do not prompt for hardware devices, it won't work.
		promptWebauthn = false
	}

	// Tweak enabled/disabled methods according to opts.
	switch {
	case promptTOTP && c.PreferOTP:
		promptWebauthn = false
	case promptWebauthn && c.AuthenticatorAttachment != wancli.AttachmentAuto:
		// Prefer Webauthn if an specific attachment was requested.
		promptTOTP = false
	case promptWebauthn && !c.AllowStdinHijack:
		// Use strongest auth if hijack is not allowed.
		promptTOTP = false
	}

	return RunOpts{promptTOTP, promptWebauthn}, nil
}

func (c PromptConfig) GetWebauthnOrigin() string {
	if !strings.HasPrefix(c.ProxyAddress, "https://") {
		return "https://" + c.ProxyAddress
	}
	return c.ProxyAddress
}

// MFAGoroutineResponse is an MFA goroutine response.
type MFAGoroutineResponse struct {
	Resp *proto.MFAAuthenticateResponse
	Err  error
}

// HandleMFAGoroutines spawns MFA prompt goroutines and returns the first successful response,
// terminating error, or an aggregated error if they all fail.
func HandleMFAGoroutines(ctx context.Context, startGoroutines func(context.Context, *sync.WaitGroup, chan<- MFAGoroutineResponse)) (*proto.MFAAuthenticateResponse, error) {
	respC := make(chan MFAGoroutineResponse, 2)
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		// wait for all goroutines to complete to ensure there are no leaks.
		wg.Wait()
	}()

	startGoroutines(ctx, &wg, respC)

	// Wait for spawned goroutines above to complete, then close respC.
	go func() {
		wg.Wait()
		close(respC)
	}()

	// Wait for a successful response, or terminating error, from the spawned goroutines.
	// The goroutine above will ensure the response channel is closed once all goroutines are done.
	var errs []error
	for resp := range respC {
		switch err := resp.Err; {
		case errors.Is(err, wancli.ErrUsingNonRegisteredDevice):
			// Surface error immediately.
			return nil, trace.Wrap(resp.Err)
		case err != nil:
			errs = append(errs, err)
			// Continue to give the other authn goroutine a chance to succeed.
			// If both have failed, this will exit the loop.
			continue
		}

		// Return successful response.
		return resp.Resp, nil
	}

	return nil, trace.Wrap(trace.NewAggregate(errs...), "failed to authenticate using available MFA devices")
}
