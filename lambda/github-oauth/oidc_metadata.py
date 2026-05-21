"""
OIDC Discovery Endpoint Handler
Provides .well-known/openid-configuration for Cognito Identity Pool integration
"""
import json
import os

ISSUER = os.environ.get('ISSUER', 'https://1yr1kjdm5j.execute-api.us-east-1.amazonaws.com')
GITHUB_CLIENT_ID = os.environ.get('GITHUB_CLIENT_ID', 'Ov23liOPNcrWFpDvtWrX')


def lambda_handler(event, context):
    """Return OIDC discovery metadata"""

    path = event.get('rawPath', '')

    if path == '/.well-known/openid-configuration':
        return handle_discovery()
    elif path == '/.well-known/jwks.json':
        return handle_jwks()
    else:
        return {
            'statusCode': 404,
            'body': json.dumps({'error': 'Not found'})
        }


def handle_discovery():
    """OIDC Discovery endpoint"""

    discovery = {
        "issuer": ISSUER,
        "authorization_endpoint": "https://github.com/login/oauth/authorize",
        "token_endpoint": f"{ISSUER}/token",
        "userinfo_endpoint": f"{ISSUER}/userinfo",
        "jwks_uri": f"{ISSUER}/.well-known/jwks.json",
        "response_types_supported": ["code", "id_token", "token id_token"],
        "subject_types_supported": ["public"],
        "id_token_signing_alg_values_supported": ["HS256"],
        "scopes_supported": ["openid", "profile", "email"],
        "token_endpoint_auth_methods_supported": ["client_secret_post"],
        "claims_supported": [
            "sub", "iss", "aud", "exp", "iat", "auth_time",
            "name", "email", "email_verified", "picture", "preferred_username"
        ]
    }

    return {
        'statusCode': 200,
        'headers': {
            'Content-Type': 'application/json',
            'Cache-Control': 'public, max-age=3600'
        },
        'body': json.dumps(discovery)
    }


def handle_jwks():
    """JWKS (JSON Web Key Set) endpoint

    For HS256 (symmetric key), we don't expose the key in JWKS.
    Cognito Identity Pools with custom OIDC providers require RS256.
    We'll need to switch to asymmetric keys for production.
    """

    # TODO: Generate RSA key pair and expose public key here
    # For now, return empty key set (will cause validation to fail)

    jwks = {
        "keys": []
    }

    return {
        'statusCode': 200,
        'headers': {
            'Content-Type': 'application/json',
            'Cache-Control': 'public, max-age=3600'
        },
        'body': json.dumps(jwks)
    }
