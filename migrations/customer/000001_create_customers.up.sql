-- Local copies of the identity provider's users (Keycloak): the customer service serves
-- profiles from here and refreshes them from Keycloak, so a Keycloak outage does not stop
-- the services that need customers' contact details.
CREATE TABLE IF NOT EXISTS customers (
    id         UUID PRIMARY KEY, -- the Keycloak user ID: the "sub" claim of access tokens
    username   TEXT        NOT NULL,
    email      TEXT        NOT NULL DEFAULT '',
    first_name TEXT        NOT NULL DEFAULT '',
    last_name  TEXT        NOT NULL DEFAULT '',
    synced_at  TIMESTAMPTZ NOT NULL, -- when the copy was last refreshed from Keycloak
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
