# fetch credentials
username=slacoin
password=$(pass nuxeo/ldap)

# base uris
rest-url=https://jira.nuxeo.com/rest
api-url=$(rest-uri)/api

# fetch token
token=$(capi -s \                                                                                                      --data "{\"username\":\"${username}\", \"password\":\"${password}\"}" \
	     --header "Content-Type: application/json" \
	     --request POST \
	     ${rest-uri}/auth/1/session | jq -r .session.value)

# print all fields
curi --request GET \
  --uri $(rest-api)/2/field \
  --header 'content-type: application/json' \
  --header 'user-agent: vscode-restclient'
  --cookie "JSESSIONID=${token}" | jq -r '.[] | {id, name}'


# print selected field
curi --silent --request GET \
  --uri https://jira.nuxeo.com/rest/api/2/field \
  --header 'content-type: application/json' \
  --header 'user-agent: vscode-restclient' | jq -r '.[] | select(.name == "Tags") | {id, name}'
{
  "id": "customfield_10080",
  "name": "Tags"
}
     

