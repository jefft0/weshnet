package weshnet

import (
	"context"
	"fmt"
	"sync"

	"github.com/gogo/protobuf/proto"
	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"

	ipfslog "berty.tech/go-ipfs-log"
	"berty.tech/go-orbit-db/iface"
	"berty.tech/weshnet/pkg/cryptoutil"
	"berty.tech/weshnet/pkg/errcode"
	"berty.tech/weshnet/pkg/protocoltypes"
	"berty.tech/weshnet/pkg/secretstore"
)

// FIXME: replace members, devices, sentSecrets, contacts and groups by a circular buffer to avoid an attack by RAM saturation
type metadataStoreIndex struct {
	members                  map[string][]secretstore.MemberDevice
	devices                  map[string]secretstore.MemberDevice
	handledEvents            map[string]struct{}
	sentSecrets              map[string]struct{}
	admins                   map[crypto.PubKey]struct{}
	contacts                 map[string]*AccountContact
	contactsFromGroupPK      map[string]*AccountContact
	groups                   map[string]*accountGroup
	serviceTokens            map[string]*protocoltypes.ServiceToken
	contactRequestMetadata   map[string][]byte
	verifiedCredentials      []*protocoltypes.AccountVerifiedCredentialRegistered
	contactRequestSeed       []byte
	contactRequestEnabled    *bool
	eventHandlers            map[protocoltypes.EventType][]func(event proto.Message) error
	postIndexActions         []func() error
	eventsContactAddAliasKey []*protocoltypes.ContactAliasKeyAdded
	ownAliasKeySent          bool
	otherAliasKey            []byte
	group                    *protocoltypes.Group
	ownMemberDevice          secretstore.MemberDevice
	secretStore              secretstore.SecretStore
	ctx                      context.Context
	lock                     sync.RWMutex
	logger                   *zap.Logger
}

func (m *metadataStoreIndex) Get(key string) interface{} {
	return nil
}

func (m *metadataStoreIndex) setLogger(logger *zap.Logger) {
	if logger == nil {
		return
	}

	m.logger = logger
}

func (m *metadataStoreIndex) UpdateIndex(log ipfslog.Log, _ []ipfslog.Entry) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	entries := log.GetEntries().Slice()

	// Resetting state
	m.contacts = map[string]*AccountContact{}
	m.contactsFromGroupPK = map[string]*AccountContact{}
	m.groups = map[string]*accountGroup{}
	m.serviceTokens = map[string]*protocoltypes.ServiceToken{}
	m.contactRequestMetadata = map[string][]byte{}
	m.contactRequestEnabled = nil
	m.contactRequestSeed = []byte(nil)
	m.verifiedCredentials = nil
	m.handledEvents = map[string]struct{}{}

	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]

		_, alreadyHandledEvent := m.handledEvents[e.GetHash().String()]

		// TODO: improve account events handling
		if m.group.GroupType != protocoltypes.GroupTypeAccount && alreadyHandledEvent {
			continue
		}

		metaEvent, event, err := openMetadataEntry(log, e, m.group)
		if err != nil {
			m.logger.Error("unable to open metadata entry", zap.Error(err))
			continue
		}

		handlers, ok := m.eventHandlers[metaEvent.Metadata.EventType]
		if !ok {
			m.handledEvents[e.GetHash().String()] = struct{}{}
			m.logger.Error("handler for event type not found", zap.String("event-type", metaEvent.Metadata.EventType.String()))
			continue
		}

		var lastErr error

		for _, h := range handlers {
			err = h(event)
			if err != nil {
				m.logger.Error("unable to handle event", zap.Error(err))
				lastErr = err
			}
		}

		if lastErr != nil {
			m.handledEvents[e.GetHash().String()] = struct{}{}
			continue
		}

		m.handledEvents[e.GetHash().String()] = struct{}{}
	}

	for _, h := range m.postIndexActions {
		if err := h(); err != nil {
			return errcode.ErrInternal.Wrap(err)
		}
	}

	return nil
}

func (m *metadataStoreIndex) handleGroupMemberDeviceAdded(event proto.Message) error {
	e, ok := event.(*protocoltypes.GroupMemberDeviceAdded)
	if !ok {
		return errcode.ErrInvalidInput
	}

	member, err := crypto.UnmarshalEd25519PublicKey(e.MemberPK)
	if err != nil {
		return errcode.ErrDeserialization.Wrap(err)
	}

	device, err := crypto.UnmarshalEd25519PublicKey(e.DevicePK)
	if err != nil {
		return errcode.ErrDeserialization.Wrap(err)
	}

	if _, ok := m.devices[string(e.DevicePK)]; ok {
		return nil
	}

	memberDevice := secretstore.NewMemberDevice(member, device)

	m.devices[string(e.DevicePK)] = memberDevice
	m.members[string(e.MemberPK)] = append(m.members[string(e.MemberPK)], memberDevice)

	return nil
}

func (m *metadataStoreIndex) handleGroupDeviceChainKeyAdded(event proto.Message) error {
	e, ok := event.(*protocoltypes.GroupDeviceChainKeyAdded)
	if !ok {
		return errcode.ErrInvalidInput
	}

	_, err := crypto.UnmarshalEd25519PublicKey(e.DestMemberPK)
	if err != nil {
		return errcode.ErrDeserialization.Wrap(err)
	}

	senderPK, err := crypto.UnmarshalEd25519PublicKey(e.DevicePK)
	if err != nil {
		return errcode.ErrDeserialization.Wrap(err)
	}

	if m.ownMemberDevice.Device().Equals(senderPK) {
		m.sentSecrets[string(e.DestMemberPK)] = struct{}{}
	}

	return nil
}

func (m *metadataStoreIndex) getMemberByDevice(devicePublicKey crypto.PubKey) (crypto.PubKey, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	publicKeyBytes, err := devicePublicKey.Raw()
	if err != nil {
		return nil, errcode.ErrInvalidInput.Wrap(err)
	}

	return m.unsafeGetMemberByDevice(publicKeyBytes)
}

func (m *metadataStoreIndex) unsafeGetMemberByDevice(publicKeyBytes []byte) (crypto.PubKey, error) {
	if l := len(publicKeyBytes); l != cryptoutil.KeySize {
		return nil, errcode.ErrInvalidInput.Wrap(fmt.Errorf("invalid private key size, expected %d got %d", cryptoutil.KeySize, l))
	}

	device, ok := m.devices[string(publicKeyBytes)]
	if !ok {
		return nil, errcode.ErrMissingInput
	}

	return device.Member(), nil
}

func (m *metadataStoreIndex) getDevicesForMember(pk crypto.PubKey) ([]crypto.PubKey, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	id, err := pk.Raw()
	if err != nil {
		return nil, errcode.ErrInvalidInput.Wrap(err)
	}

	mds, ok := m.members[string(id)]
	if !ok {
		return nil, errcode.ErrInvalidInput
	}

	ret := make([]crypto.PubKey, len(mds))
	for i, md := range mds {
		ret[i] = md.Device()
	}

	return ret, nil
}

func (m *metadataStoreIndex) MemberCount() int {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return len(m.members)
}

func (m *metadataStoreIndex) DeviceCount() int {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return len(m.devices)
}

func (m *metadataStoreIndex) listContacts() map[string]*AccountContact {
	m.lock.RLock()
	defer m.lock.RUnlock()

	contacts := make(map[string]*AccountContact)

	for k, contact := range m.contacts {
		contacts[k] = &AccountContact{
			state: contact.state,
			contact: &protocoltypes.ShareableContact{
				PK:                   contact.contact.PK,
				PublicRendezvousSeed: contact.contact.PublicRendezvousSeed,
				Metadata:             contact.contact.Metadata,
			},
		}
	}

	return contacts
}

func (m *metadataStoreIndex) listVerifiedCredentials() []*protocoltypes.AccountVerifiedCredentialRegistered {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return m.verifiedCredentials
}

func (m *metadataStoreIndex) listMembers() []crypto.PubKey {
	m.lock.RLock()
	defer m.lock.RUnlock()

	members := make([]crypto.PubKey, len(m.members))
	i := 0

	for _, md := range m.members {
		members[i] = md[0].Member()
		i++
	}

	return members
}

func (m *metadataStoreIndex) listDevices() []crypto.PubKey {
	m.lock.RLock()
	defer m.lock.RUnlock()

	devices := make([]crypto.PubKey, len(m.devices))
	i := 0

	for _, md := range m.devices {
		devices[i] = md.Device()
		i++
	}

	return devices
}

func (m *metadataStoreIndex) areSecretsAlreadySent(pk crypto.PubKey) (bool, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	key, err := pk.Raw()
	if err != nil {
		return false, errcode.ErrInvalidInput.Wrap(err)
	}

	_, ok := m.sentSecrets[string(key)]
	return ok, nil
}

type accountGroupJoinedState uint32

const (
	accountGroupJoinedStateJoined accountGroupJoinedState = iota + 1
	accountGroupJoinedStateLeft
)

type accountGroup struct {
	state accountGroupJoinedState
	group *protocoltypes.Group
}

type AccountContact struct {
	state   protocoltypes.ContactState
	contact *protocoltypes.ShareableContact
}

func (m *metadataStoreIndex) handleGroupJoined(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountGroupJoined)
	if !ok {
		return errcode.ErrInvalidInput
	}

	_, ok = m.groups[string(evt.Group.PublicKey)]
	if ok {
		return nil
	}

	m.groups[string(evt.Group.PublicKey)] = &accountGroup{
		group: evt.Group,
		state: accountGroupJoinedStateJoined,
	}

	return nil
}

func (m *metadataStoreIndex) handleGroupLeft(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountGroupLeft)
	if !ok {
		return errcode.ErrInvalidInput
	}

	_, ok = m.groups[string(evt.GroupPK)]
	if ok {
		return nil
	}

	m.groups[string(evt.GroupPK)] = &accountGroup{
		state: accountGroupJoinedStateLeft,
	}

	return nil
}

func (m *metadataStoreIndex) handleContactRequestDisabled(event proto.Message) error {
	if m.contactRequestEnabled != nil {
		return nil
	}

	_, ok := event.(*protocoltypes.AccountContactRequestDisabled)
	if !ok {
		return errcode.ErrInvalidInput
	}

	f := false
	m.contactRequestEnabled = &f

	return nil
}

func (m *metadataStoreIndex) handleContactRequestEnabled(event proto.Message) error {
	if m.contactRequestEnabled != nil {
		return nil
	}

	_, ok := event.(*protocoltypes.AccountContactRequestEnabled)
	if !ok {
		return errcode.ErrInvalidInput
	}

	t := true
	m.contactRequestEnabled = &t

	return nil
}

func (m *metadataStoreIndex) handleContactRequestReferenceReset(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountContactRequestReferenceReset)
	if !ok {
		return errcode.ErrInvalidInput
	}

	if m.contactRequestSeed != nil {
		return nil
	}

	m.contactRequestSeed = evt.PublicRendezvousSeed

	return nil
}

func (m *metadataStoreIndex) registerContactFromGroupPK(ac *AccountContact) error {
	if m.group.GroupType != protocoltypes.GroupTypeAccount {
		return errcode.ErrGroupInvalidType
	}

	contactPK, err := crypto.UnmarshalEd25519PublicKey(ac.contact.PK)
	if err != nil {
		return errcode.ErrDeserialization.Wrap(err)
	}

	group, err := m.secretStore.GetGroupForContact(contactPK)
	if err != nil {
		return errcode.ErrOrbitDBOpen.Wrap(err)
	}

	m.contactsFromGroupPK[string(group.PublicKey)] = ac

	return nil
}

func (m *metadataStoreIndex) handleContactRequestOutgoingEnqueued(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountContactRequestOutgoingEnqueued)
	if ko := !ok || evt.Contact == nil; ko {
		return errcode.ErrInvalidInput
	}

	if _, ok := m.contacts[string(evt.Contact.PK)]; ok {
		if m.contacts[string(evt.Contact.PK)].contact.Metadata == nil {
			m.contacts[string(evt.Contact.PK)].contact.Metadata = evt.Contact.Metadata
		}

		if m.contacts[string(evt.Contact.PK)].contact.PublicRendezvousSeed == nil {
			m.contacts[string(evt.Contact.PK)].contact.PublicRendezvousSeed = evt.Contact.PublicRendezvousSeed
		}

		return nil
	}

	if data, ok := m.contactRequestMetadata[string(evt.Contact.PK)]; !ok || len(data) == 0 {
		m.contactRequestMetadata[string(evt.Contact.PK)] = evt.OwnMetadata
	}

	ac := &AccountContact{
		state: protocoltypes.ContactStateToRequest,
		contact: &protocoltypes.ShareableContact{
			PK:                   evt.Contact.PK,
			Metadata:             evt.Contact.Metadata,
			PublicRendezvousSeed: evt.Contact.PublicRendezvousSeed,
		},
	}

	m.contacts[string(evt.Contact.PK)] = ac
	err := m.registerContactFromGroupPK(ac)

	return err
}

func (m *metadataStoreIndex) handleContactRequestOutgoingSent(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountContactRequestOutgoingSent)
	if !ok {
		return errcode.ErrInvalidInput
	}

	if _, ok := m.contacts[string(evt.ContactPK)]; ok {
		return nil
	}

	ac := &AccountContact{
		state: protocoltypes.ContactStateAdded,
		contact: &protocoltypes.ShareableContact{
			PK: evt.ContactPK,
		},
	}

	m.contacts[string(evt.ContactPK)] = ac
	err := m.registerContactFromGroupPK(ac)

	return err
}

func (m *metadataStoreIndex) handleContactRequestIncomingReceived(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountContactRequestIncomingReceived)
	if !ok {
		return errcode.ErrInvalidInput
	}

	if _, ok := m.contacts[string(evt.ContactPK)]; ok {
		if m.contacts[string(evt.ContactPK)].contact.Metadata == nil {
			m.contacts[string(evt.ContactPK)].contact.Metadata = evt.ContactMetadata
		}

		if m.contacts[string(evt.ContactPK)].contact.PublicRendezvousSeed == nil {
			m.contacts[string(evt.ContactPK)].contact.PublicRendezvousSeed = evt.ContactRendezvousSeed
		}

		return nil
	}

	ac := &AccountContact{
		state: protocoltypes.ContactStateReceived,
		contact: &protocoltypes.ShareableContact{
			PK:                   evt.ContactPK,
			Metadata:             evt.ContactMetadata,
			PublicRendezvousSeed: evt.ContactRendezvousSeed,
		},
	}

	m.contacts[string(evt.ContactPK)] = ac
	err := m.registerContactFromGroupPK(ac)

	return err
}

func (m *metadataStoreIndex) handleContactRequestIncomingDiscarded(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountContactRequestIncomingDiscarded)
	if !ok {
		return errcode.ErrInvalidInput
	}

	if _, ok := m.contacts[string(evt.ContactPK)]; ok {
		return nil
	}

	ac := &AccountContact{
		state: protocoltypes.ContactStateDiscarded,
		contact: &protocoltypes.ShareableContact{
			PK: evt.ContactPK,
		},
	}

	m.contacts[string(evt.ContactPK)] = ac
	err := m.registerContactFromGroupPK(ac)

	return err
}

func (m *metadataStoreIndex) handleContactRequestIncomingAccepted(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountContactRequestIncomingAccepted)
	if !ok {
		return errcode.ErrInvalidInput
	}

	if _, ok := m.contacts[string(evt.ContactPK)]; ok {
		return nil
	}

	ac := &AccountContact{
		state: protocoltypes.ContactStateAdded,
		contact: &protocoltypes.ShareableContact{
			PK: evt.ContactPK,
		},
	}

	m.contacts[string(evt.ContactPK)] = ac
	err := m.registerContactFromGroupPK(ac)

	return err
}

func (m *metadataStoreIndex) handleContactBlocked(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountContactBlocked)
	if !ok {
		return errcode.ErrInvalidInput
	}

	if _, ok := m.contacts[string(evt.ContactPK)]; ok {
		return nil
	}

	ac := &AccountContact{
		state: protocoltypes.ContactStateBlocked,
		contact: &protocoltypes.ShareableContact{
			PK: evt.ContactPK,
		},
	}

	m.contacts[string(evt.ContactPK)] = ac
	err := m.registerContactFromGroupPK(ac)

	return err
}

func (m *metadataStoreIndex) handleContactUnblocked(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountContactUnblocked)
	if !ok {
		return errcode.ErrInvalidInput
	}

	if _, ok := m.contacts[string(evt.ContactPK)]; ok {
		return nil
	}

	ac := &AccountContact{
		state: protocoltypes.ContactStateRemoved,
		contact: &protocoltypes.ShareableContact{
			PK: evt.ContactPK,
		},
	}

	m.contacts[string(evt.ContactPK)] = ac
	err := m.registerContactFromGroupPK(ac)

	return err
}

func (m *metadataStoreIndex) handleContactAliasKeyAdded(event proto.Message) error {
	evt, ok := event.(*protocoltypes.ContactAliasKeyAdded)
	if !ok {
		return errcode.ErrInvalidInput
	}

	m.eventsContactAddAliasKey = append(m.eventsContactAddAliasKey, evt)

	return nil
}

func (m *metadataStoreIndex) listServiceTokens() []*protocoltypes.ServiceToken {
	m.lock.RLock()
	defer m.lock.RUnlock()

	ret := []*protocoltypes.ServiceToken(nil)

	for _, t := range m.serviceTokens {
		if t == nil {
			continue
		}

		ret = append(ret, t)
	}

	return ret
}

func (m *metadataStoreIndex) handleAccountServiceTokenAdded(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountServiceTokenAdded)
	if !ok {
		return errcode.ErrInvalidInput
	}

	if _, ok := m.serviceTokens[evt.ServiceToken.TokenID()]; ok {
		return nil
	}

	m.serviceTokens[evt.ServiceToken.TokenID()] = evt.ServiceToken

	return nil
}

func (m *metadataStoreIndex) handleAccountServiceTokenRemoved(event proto.Message) error {
	evt, ok := event.(*protocoltypes.AccountServiceTokenRemoved)
	if !ok {
		return errcode.ErrInvalidInput
	}

	m.serviceTokens[evt.TokenID] = nil

	return nil
}

func (m *metadataStoreIndex) handleMultiMemberInitialMember(event proto.Message) error {
	e, ok := event.(*protocoltypes.MultiMemberGroupInitialMemberAnnounced)
	if !ok {
		return errcode.ErrInvalidInput
	}

	pk, err := crypto.UnmarshalEd25519PublicKey(e.MemberPK)
	if err != nil {
		return errcode.ErrDeserialization.Wrap(err)
	}

	if _, ok := m.admins[pk]; ok {
		return errcode.ErrInternal
	}

	m.admins[pk] = struct{}{}

	return nil
}

func (m *metadataStoreIndex) handleMultiMemberGrantAdminRole(event proto.Message) error {
	// TODO:

	return nil
}

func (m *metadataStoreIndex) handleGroupMetadataPayloadSent(_ proto.Message) error {
	return nil
}

func (m *metadataStoreIndex) handleAccountVerifiedCredentialRegistered(event proto.Message) error {
	e, ok := event.(*protocoltypes.AccountVerifiedCredentialRegistered)
	if !ok {
		return errcode.ErrInvalidInput
	}

	m.verifiedCredentials = append(m.verifiedCredentials, e)

	return nil
}

func (m *metadataStoreIndex) listAdmins() []crypto.PubKey {
	m.lock.RLock()
	defer m.lock.RUnlock()

	admins := make([]crypto.PubKey, len(m.admins))
	i := 0

	for admin := range m.admins {
		admins[i] = admin
		i++
	}

	return admins
}

func (m *metadataStoreIndex) listOtherMembersDevices() []crypto.PubKey {
	m.lock.RLock()
	defer m.lock.RUnlock()

	if m.ownMemberDevice == nil || m.ownMemberDevice.Member() == nil {
		return nil
	}

	ownMemberPK, err := m.ownMemberDevice.Member().Raw()
	if err != nil {
		m.logger.Warn("unable to serialize member pubkey", zap.Error(err))
		return nil
	}

	devices := []crypto.PubKey(nil)
	for pk, devicesForMember := range m.members {
		if string(ownMemberPK) == pk {
			continue
		}

		for _, md := range devicesForMember {
			devices = append(devices, md.Device())
		}
	}

	return devices
}

func (m *metadataStoreIndex) contactRequestsEnabled() bool {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return m.contactRequestEnabled != nil && *m.contactRequestEnabled
}

func (m *metadataStoreIndex) contactRequestsSeed() []byte {
	m.lock.RLock()
	defer m.lock.RUnlock()

	return m.contactRequestSeed
}

func (m *metadataStoreIndex) getContact(pk crypto.PubKey) (*AccountContact, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	bytes, err := pk.Raw()
	if err != nil {
		return nil, errcode.ErrSerialization.Wrap(err)
	}

	contact, ok := m.contacts[string(bytes)]
	if !ok {
		return nil, errcode.ErrMissingMapKey.Wrap(err)
	}

	return contact, nil
}

func (m *metadataStoreIndex) postHandlerSentAliases() error {
	for _, evt := range m.eventsContactAddAliasKey {
		memberPublicKey, err := m.unsafeGetMemberByDevice(evt.DevicePK)
		if err != nil {
			return fmt.Errorf("couldn't get member for device")
		}

		if memberPublicKey.Equals(m.ownMemberDevice.Member()) {
			m.ownAliasKeySent = true
			continue
		}

		if l := len(evt.AliasPK); l != cryptoutil.KeySize {
			return errcode.ErrInvalidInput.Wrap(fmt.Errorf("invalid alias key size, expected %d, got %d", cryptoutil.KeySize, l))
		}

		m.otherAliasKey = evt.AliasPK
	}

	m.eventsContactAddAliasKey = nil

	return nil
}

// nolint:staticcheck
// newMetadataIndex returns a new index to manage the list of the group members
func newMetadataIndex(ctx context.Context, g *protocoltypes.Group, md secretstore.MemberDevice, secretStore secretstore.SecretStore) iface.IndexConstructor {
	return func(publicKey []byte) iface.StoreIndex {
		m := &metadataStoreIndex{
			members:                map[string][]secretstore.MemberDevice{},
			devices:                map[string]secretstore.MemberDevice{},
			admins:                 map[crypto.PubKey]struct{}{},
			sentSecrets:            map[string]struct{}{},
			handledEvents:          map[string]struct{}{},
			contacts:               map[string]*AccountContact{},
			contactsFromGroupPK:    map[string]*AccountContact{},
			groups:                 map[string]*accountGroup{},
			serviceTokens:          map[string]*protocoltypes.ServiceToken{},
			contactRequestMetadata: map[string][]byte{},
			group:                  g,
			ownMemberDevice:        md,
			secretStore:            secretStore,
			ctx:                    ctx,
			logger:                 zap.NewNop(),
		}

		m.eventHandlers = map[protocoltypes.EventType][]func(event proto.Message) error{
			protocoltypes.EventTypeAccountContactBlocked:                  {m.handleContactBlocked},
			protocoltypes.EventTypeAccountContactRequestDisabled:          {m.handleContactRequestDisabled},
			protocoltypes.EventTypeAccountContactRequestEnabled:           {m.handleContactRequestEnabled},
			protocoltypes.EventTypeAccountContactRequestIncomingAccepted:  {m.handleContactRequestIncomingAccepted},
			protocoltypes.EventTypeAccountContactRequestIncomingDiscarded: {m.handleContactRequestIncomingDiscarded},
			protocoltypes.EventTypeAccountContactRequestIncomingReceived:  {m.handleContactRequestIncomingReceived},
			protocoltypes.EventTypeAccountContactRequestOutgoingEnqueued:  {m.handleContactRequestOutgoingEnqueued},
			protocoltypes.EventTypeAccountContactRequestOutgoingSent:      {m.handleContactRequestOutgoingSent},
			protocoltypes.EventTypeAccountContactRequestReferenceReset:    {m.handleContactRequestReferenceReset},
			protocoltypes.EventTypeAccountContactUnblocked:                {m.handleContactUnblocked},
			protocoltypes.EventTypeAccountGroupJoined:                     {m.handleGroupJoined},
			protocoltypes.EventTypeAccountGroupLeft:                       {m.handleGroupLeft},
			protocoltypes.EventTypeContactAliasKeyAdded:                   {m.handleContactAliasKeyAdded},
			protocoltypes.EventTypeGroupDeviceChainKeyAdded:               {m.handleGroupDeviceChainKeyAdded},
			protocoltypes.EventTypeGroupMemberDeviceAdded:                 {m.handleGroupMemberDeviceAdded},
			protocoltypes.EventTypeMultiMemberGroupAdminRoleGranted:       {m.handleMultiMemberGrantAdminRole},
			protocoltypes.EventTypeMultiMemberGroupInitialMemberAnnounced: {m.handleMultiMemberInitialMember},
			protocoltypes.EventTypeAccountServiceTokenAdded:               {m.handleAccountServiceTokenAdded},
			protocoltypes.EventTypeAccountServiceTokenRemoved:             {m.handleAccountServiceTokenRemoved},
			protocoltypes.EventTypeGroupMetadataPayloadSent:               {m.handleGroupMetadataPayloadSent},
			protocoltypes.EventTypeAccountVerifiedCredentialRegistered:    {m.handleAccountVerifiedCredentialRegistered},
		}

		m.postIndexActions = []func() error{
			m.postHandlerSentAliases,
		}

		return m
	}
}

var _ iface.StoreIndex = &metadataStoreIndex{}
