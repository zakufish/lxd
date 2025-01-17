package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/canonical/lxd/client"
	"github.com/canonical/lxd/lxd/auth"
	"github.com/canonical/lxd/lxd/cluster"
	"github.com/canonical/lxd/lxd/db"
	dbCluster "github.com/canonical/lxd/lxd/db/cluster"
	"github.com/canonical/lxd/lxd/lifecycle"
	"github.com/canonical/lxd/lxd/request"
	"github.com/canonical/lxd/lxd/response"
	"github.com/canonical/lxd/lxd/util"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	"github.com/canonical/lxd/shared/entity"
)

var authGroupsCmd = APIEndpoint{
	Name: "auth_groups",
	Path: "auth/groups",
	Get: APIEndpointAction{
		Handler:       getAuthGroups,
		AccessHandler: allowAuthenticated,
	},
	Post: APIEndpointAction{
		Handler:       createAuthGroup,
		AccessHandler: allowPermission(entity.TypeServer, auth.EntitlementCanCreateGroups),
	},
}

var authGroupCmd = APIEndpoint{
	Name: "auth_group",
	Path: "auth/groups/{groupName}",
	Get: APIEndpointAction{
		Handler:       getAuthGroup,
		AccessHandler: allowPermission(entity.TypeAuthGroup, auth.EntitlementCanView, "groupName"),
	},
	Put: APIEndpointAction{
		Handler:       updateAuthGroup,
		AccessHandler: allowPermission(entity.TypeAuthGroup, auth.EntitlementCanEdit, "groupName"),
	},
	Post: APIEndpointAction{
		Handler:       renameAuthGroup,
		AccessHandler: allowPermission(entity.TypeAuthGroup, auth.EntitlementCanEdit, "groupName"),
	},
	Delete: APIEndpointAction{
		Handler:       deleteAuthGroup,
		AccessHandler: allowPermission(entity.TypeAuthGroup, auth.EntitlementCanDelete, "groupName"),
	},
	Patch: APIEndpointAction{
		Handler:       patchAuthGroup,
		AccessHandler: allowPermission(entity.TypeAuthGroup, auth.EntitlementCanEdit, "groupName"),
	},
}

func validateGroupName(name string) error {
	if name == "" {
		return api.StatusErrorf(http.StatusBadRequest, "Group name cannot be empty")
	}

	if strings.Contains(name, "/") {
		return api.StatusErrorf(http.StatusBadRequest, "Group name cannot contain a forward slash")
	}

	if strings.Contains(name, ":") {
		return api.StatusErrorf(http.StatusBadRequest, "Group name cannot contain a colon")
	}

	return nil
}

// swagger:operation GET /1.0/auth/groups auth_groups auth_groups_get
//
//	Get the groups
//
//	Returns a list of authorization groups (URLs).
//
//	---
//	produces:
//	  - application/json
//	responses:
//	  "200":
//	    description: API endpoints
//	    schema:
//	      type: object
//	      description: Sync response
//	      properties:
//	        type:
//	          type: string
//	          description: Response type
//	          example: sync
//	        status:
//	          type: string
//	          description: Status description
//	          example: Success
//	        status_code:
//	          type: integer
//	          description: Status code
//	          example: 200
//	        metadata:
//	          type: array
//	          description: List of endpoints
//	          items:
//	            type: string
//	          example: |-
//	            [
//	              "/1.0/auth/groups/foo",
//	              "/1.0/auth/groups/bar"
//	            ]
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"

// swagger:operation GET /1.0/auth/groups?recursion=1 auth_groups auth_groups_get_recursion1
//
//	Get the groups
//
//	Returns a list of authorization groups.
//
//	---
//	produces:
//	  - application/json
//	responses:
//	  "200":
//	    description: API endpoints
//	    schema:
//	      type: object
//	      description: Sync response
//	      properties:
//	        type:
//	          type: string
//	          description: Response type
//	          example: sync
//	        status:
//	          type: string
//	          description: Status description
//	          example: Success
//	        status_code:
//	          type: integer
//	          description: Status code
//	          example: 200
//	        metadata:
//	          type: array
//	          description: List of auth groups
//	          items:
//	            $ref: "#/definitions/AuthGroup"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func getAuthGroups(d *Daemon, r *http.Request) response.Response {
	recursion := request.QueryParam(r, "recursion")
	s := d.State()

	hasPermission, err := s.Authorizer.GetPermissionChecker(r.Context(), r, auth.EntitlementCanViewGroups, entity.TypeAuthGroup)
	if err != nil {
		return response.SmartError(fmt.Errorf("Failed to get a permission checker: %w", err))
	}

	var groups []dbCluster.AuthGroup
	groupsPermissions := make(map[int][]dbCluster.Permission)
	groupsIdentities := make(map[int][]dbCluster.Identity)
	groupsIdentityProviderGroups := make(map[int][]dbCluster.IdentityProviderGroup)
	entityURLs := make(map[entity.Type]map[int]*api.URL)
	err = d.db.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
		allGroups, err := dbCluster.GetAuthGroups(ctx, tx.Tx())
		if err != nil {
			return err
		}

		groups = make([]dbCluster.AuthGroup, 0, len(groups))
		for _, group := range allGroups {
			if hasPermission(entity.AuthGroupURL(group.Name)) {
				groups = append(groups, group)
			}
		}

		if len(groups) == 0 {
			return nil
		}

		if recursion == "1" {
			// If recursing, we need all identities for all groups, all IDP groups for all groups,
			// all permissions for all groups, and finally the URLs that those permissions apply to.
			groupsIdentities, err = dbCluster.GetAllIdentitiesByAuthGroupIDs(ctx, tx.Tx())
			if err != nil {
				return err
			}

			groupsIdentityProviderGroups, err = dbCluster.GetAllIdentityProviderGroupsByGroupIDs(ctx, tx.Tx())
			if err != nil {
				return err
			}

			groupsPermissions, err = dbCluster.GetAllPermissionsByAuthGroupIDs(ctx, tx.Tx())
			if err != nil {
				return err
			}

			// allGroupPermissions is a de-duplicated slice of permissions.
			var allGroupPermissions []dbCluster.Permission
			for _, groupPermissions := range groupsPermissions {
				for _, permission := range groupPermissions {
					if !shared.ValueInSlice(permission, allGroupPermissions) {
						allGroupPermissions = append(allGroupPermissions, permission)
					}
				}
			}

			// EntityURLs is a map of entity type, to entity ID, to api.URL.
			entityURLs, err = dbCluster.GetPermissionEntityURLs(ctx, tx.Tx(), allGroupPermissions)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	if recursion == "1" {
		apiGroups := make([]api.AuthGroup, 0, len(groups))
		for _, group := range groups {
			var apiPermissions []api.Permission

			// The group may not have any permissions.
			permissions, ok := groupsPermissions[group.ID]
			if ok {
				apiPermissions = make([]api.Permission, 0, len(permissions))
				for _, permission := range permissions {
					// Expect to find any permissions in the entity URL map by its entity type and entity ID.
					entityIDToURL, ok := entityURLs[entity.Type(permission.EntityType)]
					if !ok {
						return response.InternalError(fmt.Errorf("Entity URLs missing for permissions with entity type %q", permission.EntityType))
					}

					apiURL, ok := entityIDToURL[permission.EntityID]
					if !ok {
						return response.InternalError(fmt.Errorf("Entity URL missing for permission with entity type %q and entity ID `%d`", permission.EntityType, permission.EntityID))
					}

					apiPermissions = append(apiPermissions, api.Permission{
						EntityType:      string(permission.EntityType),
						EntityReference: apiURL.String(),
						Entitlement:     string(permission.Entitlement),
					})
				}
			}

			apiIdentities := make([]api.Identity, 0, len(groupsIdentities[group.ID]))
			for _, identity := range groupsIdentities[group.ID] {
				apiIdentities = append(apiIdentities, api.Identity{
					AuthenticationMethod: string(identity.AuthMethod),
					Type:                 string(identity.Type),
					Identifier:           identity.Identifier,
					Name:                 identity.Name,
				})
			}

			idpGroups := make([]string, 0, len(groupsIdentityProviderGroups[group.ID]))
			for _, idpGroup := range groupsIdentityProviderGroups[group.ID] {
				idpGroups = append(idpGroups, idpGroup.Name)
			}

			apiGroups = append(apiGroups, api.AuthGroup{
				AuthGroupsPost: api.AuthGroupsPost{
					AuthGroupPost: api.AuthGroupPost{Name: group.Name},
					AuthGroupPut: api.AuthGroupPut{
						Description: group.Description,
						Permissions: apiPermissions,
					},
				},
				Identities:             apiIdentities,
				IdentityProviderGroups: idpGroups,
			})
		}

		return response.SyncResponse(true, apiGroups)
	}

	groupURLs := make([]string, 0, len(groups))
	for _, group := range groups {
		groupURLs = append(groupURLs, entity.AuthGroupURL(group.Name).String())
	}

	return response.SyncResponse(true, groupURLs)
}

// swagger:operation POST /1.0/auth/groups auth_groups auth_groups_post
//
//	Create a new authorization group
//
//	Creates a new authorization group.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: group
//	    description: Group request
//	    required: true
//	    schema:
//	      $ref: "#/definitions/AuthGroupsPost"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func createAuthGroup(d *Daemon, r *http.Request) response.Response {
	var group api.AuthGroupsPost
	err := json.NewDecoder(r.Body).Decode(&group)
	if err != nil {
		return response.BadRequest(fmt.Errorf("Invalid request body: %w", err))
	}

	err = validateGroupName(group.Name)
	if err != nil {
		return response.SmartError(err)
	}

	err = validatePermissions(group.Permissions)
	if err != nil {
		return response.SmartError(err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	s := d.State()
	err = s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		groupID, err := dbCluster.CreateAuthGroup(ctx, tx.Tx(), dbCluster.AuthGroup{
			Name:        group.Name,
			Description: group.Description,
		})
		if err != nil {
			return err
		}

		permissionIDs, err := upsertPermissions(ctx, tx.Tx(), group.Permissions)
		if err != nil {
			return err
		}

		err = dbCluster.SetAuthGroupPermissions(ctx, tx.Tx(), int(groupID), permissionIDs)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Send a lifecycle event for the group creation
	lc := lifecycle.AuthGroupCreated.Event(group.Name, request.CreateRequestor(r), nil)
	s.Events.SendLifecycle(api.ProjectDefaultName, lc)

	return response.SyncResponseLocation(true, nil, entity.AuthGroupURL(group.Name).String())
}

// swagger:operation GET /1.0/auth/groups/{groupName} auth_groups auth_group_get
//
//	Get the authorization group
//
//	Gets a specific authorization group.
//
//	---
//	produces:
//	  - application/json
//	responses:
//	  "200":
//	    schema:
//	      type: object
//	      description: Sync response
//	      properties:
//	        type:
//	          type: string
//	          description: Response type
//	          example: sync
//	        status:
//	          type: string
//	          description: Status description
//	          example: Success
//	        status_code:
//	          type: integer
//	          description: Status code
//	          example: 200
//	        metadata:
//	          $ref: "#/definitions/AuthGroup"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func getAuthGroup(d *Daemon, r *http.Request) response.Response {
	groupName, err := url.PathUnescape(mux.Vars(r)["groupName"])
	if err != nil {
		return response.SmartError(err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var apiGroup *api.AuthGroup
	s := d.State()
	err = s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		group, err := dbCluster.GetAuthGroup(ctx, tx.Tx(), groupName)
		if err != nil {
			return err
		}

		apiGroup, err = group.ToAPI(ctx, tx.Tx())
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponseETag(true, *apiGroup, *apiGroup)
}

// swagger:operation PUT /1.0/auth/groups/{groupName} auth_groups auth_group_put
//
//	Update the authorization group
//
//	Replaces the editable fields of an authorization group
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: group
//	    description: Update request
//	    schema:
//	      $ref: "#/definitions/AuthGroupPut"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func updateAuthGroup(d *Daemon, r *http.Request) response.Response {
	groupName, err := url.PathUnescape(mux.Vars(r)["groupName"])
	if err != nil {
		return response.SmartError(err)
	}

	var groupPut api.AuthGroupPut
	err = json.NewDecoder(r.Body).Decode(&groupPut)
	if err != nil {
		return response.BadRequest(fmt.Errorf("Invalid request body: %w", err))
	}

	err = validatePermissions(groupPut.Permissions)
	if err != nil {
		return response.SmartError(err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	s := d.State()
	err = s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		group, err := dbCluster.GetAuthGroup(ctx, tx.Tx(), groupName)
		if err != nil {
			return err
		}

		apiGroup, err := group.ToAPI(ctx, tx.Tx())
		if err != nil {
			return err
		}

		err = util.EtagCheck(r, *apiGroup)
		if err != nil {
			return err
		}

		err = dbCluster.UpdateAuthGroup(ctx, tx.Tx(), groupName, dbCluster.AuthGroup{
			Name:        groupName,
			Description: groupPut.Description,
		})
		if err != nil {
			return err
		}

		permissionIDs, err := upsertPermissions(ctx, tx.Tx(), groupPut.Permissions)
		if err != nil {
			return err
		}

		err = dbCluster.SetAuthGroupPermissions(ctx, tx.Tx(), group.ID, permissionIDs)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Send a lifecycle event for the group update
	lc := lifecycle.AuthGroupUpdated.Event(groupName, request.CreateRequestor(r), nil)
	s.Events.SendLifecycle(api.ProjectDefaultName, lc)

	return response.EmptySyncResponse
}

// swagger:operation PATCH /1.0/auth/groups/{groupName} auth_groups auth_group_patch
//
//	Partially update the authorization group
//
//	Updates the editable fields of an authorization group
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: group
//	    description: Update request
//	    schema:
//	      $ref: "#/definitions/AuthGroupPut"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func patchAuthGroup(d *Daemon, r *http.Request) response.Response {
	groupName, err := url.PathUnescape(mux.Vars(r)["groupName"])
	if err != nil {
		return response.SmartError(err)
	}

	var groupPut api.AuthGroupPut
	err = json.NewDecoder(r.Body).Decode(&groupPut)
	if err != nil {
		return response.BadRequest(fmt.Errorf("Invalid request body: %w", err))
	}

	err = validatePermissions(groupPut.Permissions)
	if err != nil {
		return response.SmartError(err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	s := d.State()
	err = s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		group, err := dbCluster.GetAuthGroup(ctx, tx.Tx(), groupName)
		if err != nil {
			return err
		}

		apiGroup, err := group.ToAPI(ctx, tx.Tx())
		if err != nil {
			return err
		}

		err = util.EtagCheck(r, *apiGroup)
		if err != nil {
			return err
		}

		if groupPut.Description != "" {
			err = dbCluster.UpdateAuthGroup(ctx, tx.Tx(), groupName, dbCluster.AuthGroup{
				Name:        groupName,
				Description: groupPut.Description,
			})
			if err != nil {
				return err
			}
		}

		newPermissions := make([]api.Permission, 0, len(groupPut.Permissions))
		for _, permission := range groupPut.Permissions {
			if !shared.ValueInSlice(permission, apiGroup.Permissions) {
				newPermissions = append(newPermissions, permission)
			}
		}

		permissionIDs, err := upsertPermissions(ctx, tx.Tx(), newPermissions)
		if err != nil {
			return err
		}

		err = dbCluster.SetAuthGroupPermissions(ctx, tx.Tx(), group.ID, permissionIDs)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Send a lifecycle event for the group update
	lc := lifecycle.AuthGroupUpdated.Event(groupName, request.CreateRequestor(r), nil)
	s.Events.SendLifecycle(api.ProjectDefaultName, lc)

	return response.EmptySyncResponse
}

// swagger:operation POST /1.0/auth/groups/{groupName} auth_groups auth_group_post
//
//	Rename the authorization group
//
//	Renames the authorization group
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: group
//	    description: Update request
//	    schema:
//	      $ref: "#/definitions/AuthGroupPost"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func renameAuthGroup(d *Daemon, r *http.Request) response.Response {
	groupName, err := url.PathUnescape(mux.Vars(r)["groupName"])
	if err != nil {
		return response.SmartError(err)
	}

	var groupPost api.AuthGroupPost
	err = json.NewDecoder(r.Body).Decode(&groupPost)
	if err != nil {
		return response.BadRequest(fmt.Errorf("Invalid request body: %w", err))
	}

	err = validateGroupName(groupPost.Name)
	if err != nil {
		return response.SmartError(err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	s := d.State()
	err = s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		err = dbCluster.RenameAuthGroup(ctx, tx.Tx(), groupName, groupPost.Name)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Notify other cluster members to update their identity cache.
	notifier, err := cluster.NewNotifier(s, s.Endpoints.NetworkCert(), s.ServerCert(), cluster.NotifyAlive)
	if err != nil {
		return response.SmartError(err)
	}

	err = notifier(func(client lxd.InstanceServer) error {
		_, _, err := client.RawQuery(http.MethodPost, "/internal/identity-cache-refresh", nil, "")
		return err
	})
	if err != nil {
		return response.SmartError(err)
	}

	// When a group is renamed we need to update the list of group names associated with each identity in the cache.
	// When a group is otherwise modified, the name is unchanged, so the cache doesn't need to be updated.
	// When a group is created, no identities are a member of it yet, so the cache doesn't need to be updated.
	s.UpdateIdentityCache()

	// Send a lifecycle event for the group rename
	lc := lifecycle.AuthGroupRenamed.Event(groupPost.Name, request.CreateRequestor(r), map[string]any{"old_name": groupName})
	s.Events.SendLifecycle(api.ProjectDefaultName, lc)

	return response.SyncResponseLocation(true, nil, entity.AuthGroupURL(groupPost.Name).String())
}

// swagger:operation DELETE /1.0/auth/groups/{groupName} auth_groups auth_group_delete
//
//	Delete the authorization group
//
//	Deletes the authorization group
//
//	---
//	produces:
//	  - application/json
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func deleteAuthGroup(d *Daemon, r *http.Request) response.Response {
	groupName, err := url.PathUnescape(mux.Vars(r)["groupName"])
	if err != nil {
		return response.SmartError(err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	s := d.State()
	err = s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		return dbCluster.DeleteAuthGroup(ctx, tx.Tx(), groupName)
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Notify other cluster members to update their identity cache.
	notifier, err := cluster.NewNotifier(s, s.Endpoints.NetworkCert(), s.ServerCert(), cluster.NotifyAlive)
	if err != nil {
		return response.SmartError(err)
	}

	err = notifier(func(client lxd.InstanceServer) error {
		_, _, err := client.RawQuery(http.MethodPost, "/internal/identity-cache-refresh", nil, "")
		return err
	})
	if err != nil {
		return response.SmartError(err)
	}

	// When a group is deleted we need to remove it from the list of groups names associated with each identity in the cache.
	// (When a group is created, nobody is a member of it yet, so the cache doesn't need to be updated).
	s.UpdateIdentityCache()

	// Send a lifecycle event for the group deletion
	lc := lifecycle.AuthGroupDeleted.Event(groupName, request.CreateRequestor(r), nil)
	s.Events.SendLifecycle(api.ProjectDefaultName, lc)

	return response.EmptySyncResponse
}

// validatePermissions checks that a) the entity type exists, b) the entitlement exists, c) then entity type matches the
// entity reference (URL), and d) that the entitlement is valid for the entity type.
func validatePermissions(permissions []api.Permission) error {
	for _, permission := range permissions {
		entityType := entity.Type(permission.EntityType)
		err := entityType.Validate()
		if err != nil {
			return api.StatusErrorf(http.StatusBadRequest, "Failed to validate entity type for permission with entity reference %q and entitlement %q: %v", permission.EntityReference, permission.Entitlement, err)
		}

		entitlement := auth.Entitlement(permission.Entitlement)
		err = auth.Validate(entitlement)
		if err != nil {
			return api.StatusErrorf(http.StatusBadRequest, "Failed to validate entitlement for permission with entity reference %q and entitlement %q: %v", permission.EntityReference, permission.Entitlement, err)
		}

		u, err := url.Parse(permission.EntityReference)
		if err != nil {
			return api.StatusErrorf(http.StatusBadRequest, "Failed to parse permission with entity reference %q and entitlement %q: %v", permission.EntityReference, permission.Entitlement, err)
		}

		referenceEntityType, _, _, _, err := entity.ParseURL(*u)
		if err != nil {
			return api.StatusErrorf(http.StatusBadRequest, "Failed to parse permission with entity reference %q and entitlement %q: %v", permission.EntityReference, permission.Entitlement, err)
		}

		if entityType != referenceEntityType {
			return api.StatusErrorf(http.StatusBadRequest, "Failed to parse permission with entity reference %q and entitlement %q: Entity type does not correspond to entity reference", permission.EntityReference, permission.Entitlement)
		}

		err = auth.ValidateEntitlement(entityType, entitlement)
		if err != nil {
			return api.StatusErrorf(http.StatusBadRequest, "Failed to validate group permission with entity reference %q and entitlement %q: %v", permission.EntityReference, permission.Entitlement, err)
		}
	}

	return nil
}

// upsertPermissions resolves the URLs of each permission to an entity ID and checks if the permission already
// exists (it may be assigned to another group already). If the permission does not already exist, it is created.
// A slice of permission IDs is returned that can be used to associate these permissions to a group.
func upsertPermissions(ctx context.Context, tx *sql.Tx, permissions []api.Permission) ([]int, error) {
	entityReferences := make(map[*api.URL]*dbCluster.EntityRef, len(permissions))
	permissionToURL := make(map[api.Permission]*api.URL, len(permissions))
	for _, permission := range permissions {
		u, err := url.Parse(permission.EntityReference)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse permission entity reference: %w", err)
		}

		apiURL := &api.URL{URL: *u}
		entityReferences[apiURL] = &dbCluster.EntityRef{}
		permissionToURL[permission] = apiURL
	}

	err := dbCluster.PopulateEntityReferencesFromURLs(ctx, tx, entityReferences)
	if err != nil {
		return nil, err
	}

	var permissionIDs []int
	for permission, apiURL := range permissionToURL {
		entitlement := auth.Entitlement(permission.Entitlement)
		entityType := dbCluster.EntityType(permission.EntityType)
		entityRef, ok := entityReferences[apiURL]
		if !ok {
			return nil, fmt.Errorf("Missing entity ID for permission with URL %q", permission.EntityReference)
		}

		// Get the permission, if one is found, append its ID to the slice.
		existingPermission, err := dbCluster.GetPermission(ctx, tx, entitlement, entityType, entityRef.EntityID)
		if err == nil {
			permissionIDs = append(permissionIDs, existingPermission.ID)
			continue
		} else if !api.StatusErrorCheck(err, http.StatusNotFound) {
			return nil, fmt.Errorf("Failed to check if permission with entitlement %q and URL %q already exists: %w", entitlement, permission.EntityReference, err)
		}

		// Generated "create" methods call cluster.GetPermission again to check if it exists. We already know that it doesn't exist, so create it directly.
		res, err := tx.ExecContext(ctx, `INSERT INTO permissions (entitlement, entity_type, entity_id) VALUES (?, ?, ?)`, entitlement, entityType, entityRef.EntityID)
		if err != nil {
			return nil, fmt.Errorf("Failed to insert new permission: %w", err)
		}

		lastInsertID, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("Failed to get last insert ID of new permission: %w", err)
		}

		permissionIDs = append(permissionIDs, int(lastInsertID))
	}

	return permissionIDs, nil
}
