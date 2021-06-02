package managed_groups

import (
	"context"
	"fmt"

	"github.com/hashicorp/boundary/globals"
	"github.com/hashicorp/boundary/internal/auth"
	"github.com/hashicorp/boundary/internal/auth/oidc"
	oidcstore "github.com/hashicorp/boundary/internal/auth/oidc/store"
	"github.com/hashicorp/boundary/internal/errors"
	pb "github.com/hashicorp/boundary/internal/gen/controller/api/resources/managedgroups"
	pbs "github.com/hashicorp/boundary/internal/gen/controller/api/services"
	"github.com/hashicorp/boundary/internal/perms"
	"github.com/hashicorp/boundary/internal/requests"
	"github.com/hashicorp/boundary/internal/servers/controller/common"
	"github.com/hashicorp/boundary/internal/servers/controller/handlers"
	"github.com/hashicorp/boundary/internal/types/action"
	"github.com/hashicorp/boundary/internal/types/resource"
	"github.com/hashicorp/go-bexpr"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// oidc field names
	attrFilterField = "attributes.filter"
)

var (
	oidcMaskManager handlers.MaskManager

	// IdActions contains the set of actions that can be performed on
	// individual resources
	IdActions = map[auth.Subtype]action.ActionSet{
		auth.OidcSubtype: {
			action.NoOp,
			action.Read,
			action.Update,
			action.Delete,
		},
	}

	// CollectionActions contains the set of actions that can be performed on
	// this collection
	CollectionActions = action.ActionSet{
		action.Create,
		action.List,
	}
)

func init() {
	var err error
	if oidcMaskManager, err = handlers.NewMaskManager(&oidcstore.ManagedGroup{}, &pb.ManagedGroup{}, &pb.OidcManagedGroupAttributes{}); err != nil {
		panic(err)
	}
}

// Service handles request as described by the pbs.ManagedGroupServiceServer interface.
type Service struct {
	pbs.UnimplementedManagedGroupServiceServer

	oidcRepoFn common.OidcAuthRepoFactory
}

// NewService returns a managed group service which handles managed group related requests to boundary.
func NewService(oidcRepo common.OidcAuthRepoFactory) (Service, error) {
	const op = "managed_groups.NewService"
	if oidcRepo == nil {
		return Service{}, errors.New(errors.InvalidParameter, op, "missing oidc repository provided")
	}
	return Service{oidcRepoFn: oidcRepo}, nil
}

var _ pbs.ManagedGroupServiceServer = Service{}

// ListManagedGroups implements the interface pbs.ManagedGroupsServiceServer.
func (s Service) ListManagedGroups(ctx context.Context, req *pbs.ListManagedGroupsRequest) (*pbs.ListManagedGroupsResponse, error) {
	if err := validateListRequest(req); err != nil {
		return nil, err
	}
	_, authResults := s.parentAndAuthResult(ctx, req.GetAuthMethodId(), action.List)
	if authResults.Error != nil {
		return nil, authResults.Error
	}
	ul, err := s.listFromRepo(ctx, req.GetAuthMethodId())
	if err != nil {
		return nil, err
	}
	if len(ul) == 0 {
		return &pbs.ListManagedGroupsResponse{}, nil
	}

	filter, err := handlers.NewFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	finalItems := make([]*pb.ManagedGroup, 0, len(ul))

	res := perms.Resource{
		ScopeId: authResults.Scope.Id,
		Type:    resource.ManagedGroup,
		Pin:     req.GetAuthMethodId(),
	}
	for _, mg := range ul {
		res.Id = mg.GetPublicId()
		authorizedActions := authResults.FetchActionSetForId(ctx, mg.GetPublicId(), IdActions[auth.SubtypeFromId(mg.GetPublicId())], auth.WithResource(&res)).Strings()
		if len(authorizedActions) == 0 {
			continue
		}

		outputFields := authResults.FetchOutputFields(res, action.List).SelfOrDefaults(authResults.UserId)
		outputOpts := make([]handlers.Option, 0, 3)
		outputOpts = append(outputOpts, handlers.WithOutputFields(&outputFields))
		if outputFields.Has(globals.ScopeField) {
			outputOpts = append(outputOpts, handlers.WithScope(authResults.Scope))
		}
		if outputFields.Has(globals.AuthorizedActionsField) {
			outputOpts = append(outputOpts, handlers.WithAuthorizedActions(authorizedActions))
		}

		item, err := toProto(ctx, mg, outputOpts...)
		if err != nil {
			return nil, err
		}

		// This comes last so that we can use item fields in the filter after
		// the allowed fields are populated above
		if filter.Match(item) {
			finalItems = append(finalItems, item)
		}
	}
	return &pbs.ListManagedGroupsResponse{Items: finalItems}, nil
}

// GetManagedGroup implements the interface pbs.ManagedGroupServiceServer.
func (s Service) GetManagedGroup(ctx context.Context, req *pbs.GetManagedGroupRequest) (*pbs.GetManagedGroupResponse, error) {
	const op = "managed_groups.(Service).GetManagedGroup"

	if err := validateGetRequest(req); err != nil {
		return nil, err
	}

	_, authResults := s.parentAndAuthResult(ctx, req.GetId(), action.Read)
	if authResults.Error != nil {
		return nil, authResults.Error
	}
	mg, err := s.getFromRepo(ctx, req.GetId())
	if err != nil {
		return nil, err
	}

	outputFields, ok := requests.OutputFields(ctx)
	if !ok {
		return nil, errors.New(errors.Internal, op, "no request context found")
	}

	outputOpts := make([]handlers.Option, 0, 3)
	outputOpts = append(outputOpts, handlers.WithOutputFields(&outputFields))
	if outputFields.Has(globals.ScopeField) {
		outputOpts = append(outputOpts, handlers.WithScope(authResults.Scope))
	}
	if outputFields.Has(globals.AuthorizedActionsField) {
		outputOpts = append(outputOpts, handlers.WithAuthorizedActions(authResults.FetchActionSetForId(ctx, mg.GetPublicId(), IdActions[auth.SubtypeFromId(mg.GetPublicId())]).Strings()))
	}

	item, err := toProto(ctx, mg, outputOpts...)
	if err != nil {
		return nil, err
	}

	return &pbs.GetManagedGroupResponse{Item: item}, nil
}

// CreateManagedGroup implements the interface pbs.ManagedGroupServiceServer.
func (s Service) CreateManagedGroup(ctx context.Context, req *pbs.CreateManagedGroupRequest) (*pbs.CreateManagedGroupResponse, error) {
	const op = "managed_groups.(Service).CreateManagedGroup"

	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	authMeth, authResults := s.parentAndAuthResult(ctx, req.GetItem().GetAuthMethodId(), action.Create)
	if authResults.Error != nil {
		return nil, authResults.Error
	}
	mg, err := s.createInRepo(ctx, authMeth, req.GetItem())
	if err != nil {
		return nil, err
	}

	outputFields, ok := requests.OutputFields(ctx)
	if !ok {
		return nil, errors.New(errors.Internal, op, "no request context found")
	}

	outputOpts := make([]handlers.Option, 0, 3)
	outputOpts = append(outputOpts, handlers.WithOutputFields(&outputFields))
	if outputFields.Has(globals.ScopeField) {
		outputOpts = append(outputOpts, handlers.WithScope(authResults.Scope))
	}
	if outputFields.Has(globals.AuthorizedActionsField) {
		outputOpts = append(outputOpts, handlers.WithAuthorizedActions(authResults.FetchActionSetForId(ctx, mg.GetPublicId(), IdActions[auth.SubtypeFromId(mg.GetPublicId())]).Strings()))
	}

	item, err := toProto(ctx, mg, outputOpts...)
	if err != nil {
		return nil, err
	}

	return &pbs.CreateManagedGroupResponse{Item: item, Uri: fmt.Sprintf("managed-groups/%s", item.GetId())}, nil
}

// UpdateManagedGroup implements the interface pbs.ManagedGroupServiceServer.
func (s Service) UpdateManagedGroup(ctx context.Context, req *pbs.UpdateManagedGroupRequest) (*pbs.UpdateManagedGroupResponse, error) {
	const op = "managed_groups.(Service).UpdateManagedGroup"

	if err := validateUpdateRequest(req); err != nil {
		return nil, err
	}

	authMeth, authResults := s.parentAndAuthResult(ctx, req.GetId(), action.Update)
	if authResults.Error != nil {
		return nil, authResults.Error
	}
	mg, err := s.updateInRepo(ctx, authResults.Scope.GetId(), authMeth.GetPublicId(), req)
	if err != nil {
		return nil, err
	}

	outputFields, ok := requests.OutputFields(ctx)
	if !ok {
		return nil, errors.New(errors.Internal, op, "no request context found")
	}

	outputOpts := make([]handlers.Option, 0, 3)
	outputOpts = append(outputOpts, handlers.WithOutputFields(&outputFields))
	if outputFields.Has(globals.ScopeField) {
		outputOpts = append(outputOpts, handlers.WithScope(authResults.Scope))
	}
	if outputFields.Has(globals.AuthorizedActionsField) {
		outputOpts = append(outputOpts, handlers.WithAuthorizedActions(authResults.FetchActionSetForId(ctx, mg.GetPublicId(), IdActions[auth.SubtypeFromId(mg.GetPublicId())]).Strings()))
	}

	item, err := toProto(ctx, mg, outputOpts...)
	if err != nil {
		return nil, err
	}

	return &pbs.UpdateManagedGroupResponse{Item: item}, nil
}

// DeleteManagedGroup implements the interface pbs.ManagedGroupServiceServer.
func (s Service) DeleteManagedGroup(ctx context.Context, req *pbs.DeleteManagedGroupRequest) (*pbs.DeleteManagedGroupResponse, error) {
	if err := validateDeleteRequest(req); err != nil {
		return nil, err
	}
	_, authResults := s.parentAndAuthResult(ctx, req.GetId(), action.Delete)
	if authResults.Error != nil {
		return nil, authResults.Error
	}
	_, err := s.deleteFromRepo(ctx, authResults.Scope.GetId(), req.GetId())
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (s Service) getFromRepo(ctx context.Context, id string) (auth.ManagedGroup, error) {
	var out auth.ManagedGroup
	switch auth.SubtypeFromId(id) {
	case auth.OidcSubtype:
		repo, err := s.oidcRepoFn()
		if err != nil {
			return nil, err
		}
		mg, err := repo.LookupManagedGroup(ctx, id)
		if err != nil {
			if errors.IsNotFoundError(err) {
				return nil, handlers.NotFoundErrorf("ManagedGroup %q doesn't exist.", id)
			}
			return nil, err
		}
		out = mg
	default:
		return nil, handlers.NotFoundErrorf("Unrecognized id.")
	}
	return out, nil
}

func (s Service) createOidcInRepo(ctx context.Context, am auth.AuthMethod, item *pb.ManagedGroup) (*oidc.ManagedGroup, error) {
	const op = "managed_groups.(Service).createOidcInRepo"
	if item == nil {
		return nil, errors.New(errors.InvalidParameter, op, "missing item")
	}
	var opts []oidc.Option
	if item.GetName() != nil {
		opts = append(opts, oidc.WithName(item.GetName().GetValue()))
	}
	if item.GetDescription() != nil {
		opts = append(opts, oidc.WithDescription(item.GetDescription().GetValue()))
	}
	attrs := &pb.OidcManagedGroupAttributes{}
	if err := handlers.StructToProto(item.GetAttributes(), attrs); err != nil {
		return nil, handlers.InvalidArgumentErrorf("Error in provided request.",
			map[string]string{"attributes": "Attribute fields do not match the expected format."})
	}
	mg, err := oidc.NewManagedGroup(am.GetPublicId(), attrs.GetFilter(), opts...)
	if err != nil {
		return nil, handlers.ApiErrorWithCodeAndMessage(codes.Internal, "Unable to build user for creation: %v.", err)
	}
	repo, err := s.oidcRepoFn()
	if err != nil {
		return nil, err
	}

	out, err := repo.CreateManagedGroup(ctx, am.GetScopeId(), mg)
	if err != nil {
		return nil, errors.Wrap(err, op, errors.WithMsg("unable to create managed group"))
	}
	if out == nil {
		return nil, handlers.ApiErrorWithCodeAndMessage(codes.Internal, "Unable to create managed group but no error returned from repository.")
	}
	return out, nil
}

func (s Service) createInRepo(ctx context.Context, am auth.AuthMethod, item *pb.ManagedGroup) (auth.ManagedGroup, error) {
	const op = "managed_groups.(Service).createInRepo"
	if item == nil {
		return nil, errors.New(errors.InvalidParameter, op, "missing item")
	}
	var out auth.ManagedGroup
	switch auth.SubtypeFromId(am.GetPublicId()) {
	case auth.OidcSubtype:
		am, err := s.createOidcInRepo(ctx, am, item)
		if err != nil {
			return nil, errors.Wrap(err, op)
		}
		if am == nil {
			return nil, handlers.ApiErrorWithCodeAndMessage(codes.Internal, "Unable to create managed group but no error returned from repository.")
		}
		out = am
	}
	return out, nil
}

func (s Service) updateOidcInRepo(ctx context.Context, scopeId, amId, id string, mask []string, item *pb.ManagedGroup) (*oidc.ManagedGroup, error) {
	const op = "managed_groups.(Service).updateOidcInRepo"
	if item == nil {
		return nil, errors.New(errors.InvalidParameter, op, "nil managed group.")
	}
	mg := oidc.AllocManagedGroup()
	mg.PublicId = id
	if item.GetName() != nil {
		mg.Name = item.GetName().GetValue()
	}
	if item.GetDescription() != nil {
		mg.Description = item.GetDescription().GetValue()
	}
	if apiAttr := item.GetAttributes(); apiAttr != nil {
		attrs := &pb.OidcManagedGroupAttributes{}
		if err := handlers.StructToProto(apiAttr, attrs); err != nil {
			return nil, handlers.InvalidArgumentErrorf("Error in provided request.",
				map[string]string{globals.AttributesField: "Attribute fields do not match the expected format."})
		}
		// Set this regardless; it'll only take effect if the masks contain the value
		mg.Filter = attrs.Filter
	}

	version := item.GetVersion()

	dbMask := oidcMaskManager.Translate(mask)
	if len(dbMask) == 0 {
		return nil, handlers.InvalidArgumentErrorf("No valid fields included in the update mask.", map[string]string{"update_mask": "No valid fields provided in the update mask."})
	}
	repo, err := s.oidcRepoFn()
	if err != nil {
		return nil, errors.Wrap(err, op)
	}
	out, rowsUpdated, err := repo.UpdateManagedGroup(ctx, scopeId, mg, version, dbMask)
	if err != nil {
		return nil, errors.Wrap(err, op, errors.WithMsg("unable to update managed group"))
	}
	if rowsUpdated == 0 {
		return nil, handlers.NotFoundErrorf("Managed Group %q doesn't exist or incorrect version provided.", id)
	}
	return out, nil
}

func (s Service) updateInRepo(ctx context.Context, scopeId, authMethodId string, req *pbs.UpdateManagedGroupRequest) (auth.ManagedGroup, error) {
	const op = "managed_groups.(Service).updateInRepo"
	var out auth.ManagedGroup
	switch auth.SubtypeFromId(req.GetId()) {
	case auth.OidcSubtype:
		mg, err := s.updateOidcInRepo(ctx, scopeId, authMethodId, req.GetId(), req.GetUpdateMask().GetPaths(), req.GetItem())
		if err != nil {
			return nil, errors.Wrap(err, op)
		}
		if mg == nil {
			return nil, handlers.ApiErrorWithCodeAndMessage(codes.Internal, "Unable to update managed group but no error returned from repository.")
		}
		out = mg
	}
	return out, nil
}

func (s Service) deleteFromRepo(ctx context.Context, scopeId, id string) (bool, error) {
	const op = "managed_groups.(Service).deleteFromRepo"
	var rows int
	var err error
	switch auth.SubtypeFromId(id) {
	case auth.OidcSubtype:
		repo, iErr := s.oidcRepoFn()
		if iErr != nil {
			return false, iErr
		}
		rows, err = repo.DeleteManagedGroup(ctx, scopeId, id)
	}
	if err != nil {
		if errors.IsNotFoundError(err) {
			return false, nil
		}
		return false, errors.Wrap(err, op)
	}
	return rows > 0, nil
}

func (s Service) listFromRepo(ctx context.Context, authMethodId string) ([]auth.ManagedGroup, error) {
	const op = "managed_groups.(Service).listFromRepo"

	var outUl []auth.ManagedGroup
	switch auth.SubtypeFromId(authMethodId) {
	case auth.OidcSubtype:
		oidcRepo, err := s.oidcRepoFn()
		if err != nil {
			return nil, errors.Wrap(err, op)
		}
		oidcl, err := oidcRepo.ListManagedGroups(ctx, authMethodId)
		if err != nil {
			return nil, errors.Wrap(err, op)
		}
		for _, a := range oidcl {
			outUl = append(outUl, a)
		}
	}
	return outUl, nil
}

func (s Service) parentAndAuthResult(ctx context.Context, id string, a action.Type) (auth.AuthMethod, auth.VerifyResults) {
	res := auth.VerifyResults{}
	oidcRepo, err := s.oidcRepoFn()
	if err != nil {
		res.Error = err
		return nil, res
	}

	var parentId string
	opts := []auth.Option{auth.WithType(resource.ManagedGroup), auth.WithAction(a)}
	switch a {
	case action.List, action.Create:
		parentId = id
	default:
		switch auth.SubtypeFromId(id) {
		case auth.OidcSubtype:
			acct, err := oidcRepo.LookupManagedGroup(ctx, id)
			if err != nil {
				res.Error = err
				return nil, res
			}
			if acct == nil {
				res.Error = handlers.NotFoundError()
				return nil, res
			}
			parentId = acct.GetAuthMethodId()
		}
		opts = append(opts, auth.WithId(id))
	}

	var authMeth auth.AuthMethod
	switch auth.SubtypeFromId(parentId) {
	case auth.OidcSubtype:
		am, err := oidcRepo.LookupAuthMethod(ctx, parentId)
		if err != nil {
			res.Error = err
			return nil, res
		}
		if am == nil {
			res.Error = handlers.NotFoundError()
			return nil, res
		}
		authMeth = am
	}
	opts = append(opts, auth.WithScopeId(authMeth.GetScopeId()), auth.WithPin(parentId))
	return authMeth, auth.Verify(ctx, opts...)
}

func toProto(ctx context.Context, in auth.ManagedGroup, opt ...handlers.Option) (*pb.ManagedGroup, error) {
	opts := handlers.GetOpts(opt...)
	if opts.WithOutputFields == nil {
		return nil, handlers.ApiErrorWithCodeAndMessage(codes.Internal, "output fields not found when building managed group proto")
	}
	outputFields := *opts.WithOutputFields

	out := pb.ManagedGroup{}
	if outputFields.Has(globals.IdField) {
		out.Id = in.GetPublicId()
	}
	if outputFields.Has(globals.AuthMethodIdField) {
		out.AuthMethodId = in.GetAuthMethodId()
	}
	if outputFields.Has(globals.DescriptionField) && in.GetDescription() != "" {
		out.Description = &wrapperspb.StringValue{Value: in.GetDescription()}
	}
	if outputFields.Has(globals.NameField) && in.GetName() != "" {
		out.Name = &wrapperspb.StringValue{Value: in.GetName()}
	}
	if outputFields.Has(globals.CreatedTimeField) {
		out.CreatedTime = in.GetCreateTime().GetTimestamp()
	}
	if outputFields.Has(globals.UpdatedTimeField) {
		out.UpdatedTime = in.GetUpdateTime().GetTimestamp()
	}
	if outputFields.Has(globals.VersionField) {
		out.Version = in.GetVersion()
	}
	if outputFields.Has(globals.ScopeField) {
		out.Scope = opts.WithScope
	}
	if outputFields.Has(globals.AuthorizedActionsField) {
		out.AuthorizedActions = opts.WithAuthorizedActions
	}
	switch i := in.(type) {
	case *oidc.ManagedGroup:
		if outputFields.Has(globals.TypeField) {
			out.Type = auth.OidcSubtype.String()
		}
		if !outputFields.Has(globals.AttributesField) {
			break
		}
		attrs := &pb.OidcManagedGroupAttributes{
			Filter: i.GetFilter(),
		}
		st, err := handlers.ProtoToStruct(attrs)
		if err != nil {
			return nil, handlers.ApiErrorWithCodeAndMessage(codes.Internal, "failed building oidc attribute struct: %v", err)
		}
		out.Attributes = st
	}
	return &out, nil
}

// A validateX method should exist for each method above.  These methods do not make calls to any backing service but enforce
// requirements on the structure of the request.  They verify that:
//  * The path passed in is correctly formatted
//  * All required parameters are set
//  * There are no conflicting parameters provided
func validateGetRequest(req *pbs.GetManagedGroupRequest) error {
	const op = "managed_groups.validateGetRequest"
	if req == nil {
		return errors.New(errors.InvalidParameter, op, "nil request")
	}
	return handlers.ValidateGetRequest(handlers.NoopValidatorFn, req, oidc.ManagedGroupPrefix)
}

func validateCreateRequest(req *pbs.CreateManagedGroupRequest) error {
	const op = "managed_groups.validateCreateRequest"
	if req == nil {
		return errors.New(errors.InvalidParameter, op, "nil request")
	}
	return handlers.ValidateCreateRequest(req.GetItem(), func() map[string]string {
		badFields := map[string]string{}
		if req.GetItem().GetAuthMethodId() == "" {
			badFields[globals.AuthMethodIdField] = "This field is required."
		}
		switch auth.SubtypeFromId(req.GetItem().GetAuthMethodId()) {
		case auth.OidcSubtype:
			if req.GetItem().GetType() != "" && req.GetItem().GetType() != auth.OidcSubtype.String() {
				badFields[globals.TypeField] = "Doesn't match the parent resource's type."
			}
			attrs := &pb.OidcManagedGroupAttributes{}
			if err := handlers.StructToProto(req.GetItem().GetAttributes(), attrs); err != nil {
				badFields[globals.AttributesField] = "Attribute fields do not match the expected format."
			}
			if attrs.Filter == "" {
				badFields[attrFilterField] = "This field is required."
			} else {
				if _, err := bexpr.CreateEvaluator(attrs.Filter); err != nil {
					badFields[attrFilterField] = fmt.Sprintf("Error evaluating submitted filter expression: %v.", err)
				}
			}
		default:
			badFields[globals.AuthMethodIdField] = "Unknown auth method type from ID."
		}
		return badFields
	})
}

func validateUpdateRequest(req *pbs.UpdateManagedGroupRequest) error {
	const op = "managed_groups.validateUpdateRequest"
	if req == nil {
		return errors.New(errors.InvalidParameter, op, "nil request")
	}
	return handlers.ValidateUpdateRequest(req, req.GetItem(), func() map[string]string {
		badFields := map[string]string{}
		switch auth.SubtypeFromId(req.GetId()) {
		case auth.OidcSubtype:
			if req.GetItem().GetType() != "" && req.GetItem().GetType() != auth.OidcSubtype.String() {
				badFields[globals.TypeField] = "Cannot modify the resource type."
			}
			attrs := &pb.OidcManagedGroupAttributes{}
			if err := handlers.StructToProto(req.GetItem().GetAttributes(), attrs); err != nil {
				badFields[globals.AttributesField] = "Attribute fields do not match the expected format."
			}
			if handlers.MaskContains(req.GetUpdateMask().GetPaths(), attrFilterField) {
				if attrs.Filter == "" {
					badFields[attrFilterField] = "Field cannot be empty."
				} else {
					if _, err := bexpr.CreateEvaluator(attrs.Filter); err != nil {
						badFields[attrFilterField] = fmt.Sprintf("Error evaluating submitted filter expression: %v.", err)
					}
				}
			}
		}
		return badFields
	}, oidc.ManagedGroupPrefix)
}

func validateDeleteRequest(req *pbs.DeleteManagedGroupRequest) error {
	const op = "managed_groups.validateDeleteRequest"
	if req == nil {
		return errors.New(errors.InvalidParameter, op, "nil request")
	}
	return handlers.ValidateDeleteRequest(handlers.NoopValidatorFn, req, oidc.ManagedGroupPrefix)
}

func validateListRequest(req *pbs.ListManagedGroupsRequest) error {
	const op = "managed_groups.validateListRequest"
	if req == nil {
		return errors.New(errors.InvalidParameter, op, "nil request")
	}
	badFields := map[string]string{}
	if !handlers.ValidId(handlers.Id(req.GetAuthMethodId()), oidc.AuthMethodPrefix) {
		badFields[globals.AuthMethodIdField] = "Invalid formatted identifier."
	}
	if _, err := handlers.NewFilter(req.GetFilter()); err != nil {
		badFields[globals.FilterField] = fmt.Sprintf("This field could not be parsed. %v", err)
	}
	if len(badFields) > 0 {
		return handlers.InvalidArgumentErrorf("Error in provided request.", badFields)
	}
	return nil
}
