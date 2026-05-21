"""
GitHub OAuth to AWS Credentials Bridge Lambda
Handles GitHub OAuth 2.0 callback and returns AWS credentials via STS AssumeRole
"""
import json
import base64
import hashlib
import hmac
import ssl
import time
import urllib.request
import urllib.parse
import urllib.error
import boto3
from typing import Dict, Any

# GitHub OAuth credentials (set via environment variables)
import os
GITHUB_CLIENT_ID = os.environ.get('GITHUB_CLIENT_ID', 'Ov23liOPNcrWFpDvtWrX')
GITHUB_CLIENT_SECRET = os.environ.get('GITHUB_CLIENT_SECRET', '')
REDIRECT_URI = os.environ.get('REDIRECT_URI', 'https://api.spore.host/github/callback')
FRONTEND_CALLBACK = os.environ.get('FRONTEND_CALLBACK', 'https://spore.host/callback')

# Cross-account role for EC2 access
CROSS_ACCOUNT_ROLE_ARN = os.environ.get('CROSS_ACCOUNT_ROLE_ARN', 'arn:aws:iam::435415984226:role/SpawnDashboardCrossAccountReadRole')

# JWT signing secret (for session token)
JWT_SECRET = os.environ.get('JWT_SECRET', 'change-me-in-production')

# AWS clients
sts_client = boto3.client('sts')


def lambda_handler(event: Dict[str, Any], context: Any) -> Dict[str, Any]:
    """Main Lambda handler for GitHub OAuth callback"""

    print(f"Received event: {json.dumps(event)}")

    # Extract query parameters
    params = event.get('queryStringParameters') or {}
    code = params.get('code')
    state = params.get('state')
    error = params.get('error')

    # Handle OAuth errors from GitHub
    if error:
        error_description = params.get('error_description', 'Unknown error')
        return redirect_to_frontend_with_error(error, error_description)

    # Validate required parameters
    if not code:
        return redirect_to_frontend_with_error('invalid_request', 'Missing authorization code')

    if not state:
        return redirect_to_frontend_with_error('invalid_request', 'Missing state parameter')

    try:
        # Exchange authorization code for access token
        access_token = exchange_code_for_token(code)

        # Fetch user info from GitHub
        user_info = fetch_github_user_info(access_token)

        # Get AWS credentials via STS AssumeRole
        aws_credentials = assume_cross_account_role(user_info)

        # Generate session token for user info
        session_token = generate_session_token(user_info)

        # Redirect back to frontend with credentials
        return redirect_to_frontend_with_credentials(
            user_info, aws_credentials, session_token, state
        )

    except Exception as e:
        print(f"Error processing GitHub OAuth: {str(e)}")
        return redirect_to_frontend_with_error('server_error', str(e))


def exchange_code_for_token(code: str) -> str:
    """Exchange authorization code for access token"""

    url = 'https://github.com/login/oauth/access_token'
    data = {
        'client_id': GITHUB_CLIENT_ID,
        'client_secret': GITHUB_CLIENT_SECRET,
        'code': code,
        'redirect_uri': REDIRECT_URI
    }

    headers = {
        'Accept': 'application/json',
        'Content-Type': 'application/json'
    }

    request = urllib.request.Request(
        url,
        data=json.dumps(data).encode('utf-8'),
        headers=headers,
        method='POST'
    )

    try:
        # Use an explicit SSL context to enforce certificate verification.
        # urllib.urlopen verifies certificates by default in Python 3.4+, but
        # making it explicit documents the intent and satisfies static analysis.
        ssl_context = ssl.create_default_context()
        with urllib.request.urlopen(request, context=ssl_context) as response:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
            result = json.loads(response.read().decode('utf-8'))

            if 'error' in result:
                raise Exception(f"GitHub token exchange failed: {result.get('error_description', result['error'])}")

            access_token = result.get('access_token')
            if not access_token:
                raise Exception("No access token in GitHub response")

            return access_token

    except urllib.error.HTTPError as e:
        error_body = e.read().decode('utf-8')
        raise Exception(f"HTTP error exchanging code: {e.code} - {error_body}")


def fetch_github_user_info(access_token: str) -> Dict[str, Any]:
    """Fetch user information from GitHub API"""

    # Get primary user info
    user_url = 'https://api.github.com/user'
    headers = {
        'Authorization': f'Bearer {access_token}',
        'Accept': 'application/json',
        'User-Agent': 'Spawn-Dashboard'
    }

    request = urllib.request.Request(user_url, headers=headers)

    try:
        ssl_context = ssl.create_default_context()
        with urllib.request.urlopen(request, context=ssl_context) as response:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
            user_data = json.loads(response.read().decode('utf-8'))

        # Get user emails
        emails_url = 'https://api.github.com/user/emails'
        email_request = urllib.request.Request(emails_url, headers=headers)

        with urllib.request.urlopen(email_request, context=ssl_context) as response:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
            emails_data = json.loads(response.read().decode('utf-8'))

        # Find primary verified email
        primary_email = None
        for email in emails_data:
            if email.get('primary') and email.get('verified'):
                primary_email = email.get('email')
                break

        if not primary_email and emails_data:
            # Fall back to first verified email
            for email in emails_data:
                if email.get('verified'):
                    primary_email = email.get('email')
                    break

        # Construct OIDC-compatible user info
        return {
            'sub': str(user_data['id']),  # GitHub user ID
            'login': user_data['login'],  # GitHub username
            'name': user_data.get('name') or user_data['login'],
            'email': primary_email or f"{user_data['login']}@github.com",
            'email_verified': bool(primary_email),
            'picture': user_data.get('avatar_url'),
            'profile': user_data.get('html_url'),
            'preferred_username': user_data['login']
        }

    except urllib.error.HTTPError as e:
        error_body = e.read().decode('utf-8')
        raise Exception(f"HTTP error fetching user info: {e.code} - {error_body}")


def assume_cross_account_role(user_info: Dict[str, Any]) -> Dict[str, Any]:
    """Assume cross-account role to access EC2 resources"""

    role_session_name = f"github-{user_info['login']}-{int(time.time())}"

    try:
        response = sts_client.assume_role(
            RoleArn=CROSS_ACCOUNT_ROLE_ARN,
            RoleSessionName=role_session_name,
            DurationSeconds=3600  # 1 hour
        )

        credentials = response['Credentials']

        return {
            'accessKeyId': credentials['AccessKeyId'],
            'secretAccessKey': credentials['SecretAccessKey'],
            'sessionToken': credentials['SessionToken'],
            'expiration': int(credentials['Expiration'].timestamp() * 1000)  # milliseconds
        }

    except Exception as e:
        print(f"Error assuming role: {str(e)}")
        raise Exception(f"Failed to get AWS credentials: {str(e)}")


def generate_session_token(user_info: Dict[str, Any]) -> str:
    """Generate simple session token for user info"""

    now = int(time.time())

    # JWT Header
    header = {
        'alg': 'HS256',
        'typ': 'JWT'
    }

    # JWT Payload
    payload = {
        'iss': 'https://1yr1kjdm5j.execute-api.us-east-1.amazonaws.com',  # Issuer
        'sub': user_info['sub'],  # Subject (user ID)
        'exp': now + 3600,  # Expiration (1 hour)
        'iat': now,  # Issued at

        # User claims
        'name': user_info['name'],
        'email': user_info['email'],
        'picture': user_info.get('picture'),
        'preferred_username': user_info['preferred_username']
    }

    # Encode header and payload
    header_b64 = base64_url_encode(json.dumps(header).encode('utf-8'))
    payload_b64 = base64_url_encode(json.dumps(payload).encode('utf-8'))

    # Create signature
    message = f"{header_b64}.{payload_b64}"
    signature = hmac.new(
        JWT_SECRET.encode('utf-8'),
        message.encode('utf-8'),
        hashlib.sha256
    ).digest()
    signature_b64 = base64_url_encode(signature)

    # Construct JWT
    token = f"{header_b64}.{payload_b64}.{signature_b64}"

    return token


def base64_url_encode(data: bytes) -> str:
    """Base64 URL-safe encoding (without padding)"""
    return base64.urlsafe_b64encode(data).decode('utf-8').rstrip('=')


def redirect_to_frontend_with_credentials(
    user_info: Dict[str, Any],
    aws_credentials: Dict[str, Any],
    session_token: str,
    state: str
) -> Dict[str, Any]:
    """Redirect to frontend with AWS credentials and user info in URL fragment"""

    # Package data as base64-encoded JSON (to fit in URL fragment)
    data = {
        'provider': 'github',
        'user': {
            'id': user_info['sub'],
            'email': user_info['email'],
            'name': user_info['name'],
            'picture': user_info.get('picture')
        },
        'credentials': aws_credentials,
        'state': state
    }

    # Base64 encode the JSON
    data_json = json.dumps(data)
    data_b64 = base64.urlsafe_b64encode(data_json.encode('utf-8')).decode('utf-8')

    # Construct fragment
    fragment = f"github_auth={data_b64}"

    redirect_url = f"{FRONTEND_CALLBACK}#{fragment}"

    return {
        'statusCode': 302,
        'headers': {
            'Location': redirect_url,
            'Cache-Control': 'no-cache, no-store, must-revalidate'
        },
        'body': ''
    }


def redirect_to_frontend_with_error(error: str, description: str) -> Dict[str, Any]:
    """Redirect to frontend with error in URL fragment"""

    fragment = urllib.parse.urlencode({
        'error': error,
        'error_description': description
    })

    redirect_url = f"{FRONTEND_CALLBACK}#{fragment}"

    return {
        'statusCode': 302,
        'headers': {
            'Location': redirect_url,
            'Cache-Control': 'no-cache, no-store, must-revalidate'
        },
        'body': ''
    }
