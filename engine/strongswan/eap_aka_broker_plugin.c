/*
 * Viti Operator EAP-AKA card and P-CSCF attribute plugin for strongSwan.
 *
 * The plugin deliberately receives only one RAND/AUTN challenge from charon
 * and forwards it to the existing HTTP AKA broker.  Ki and OPc never enter
 * this process.  The broker may be backed by the local OpenStick or by a
 * selected Android authentication agent.
 *
 * SPDX-License-Identifier: GPL-2.0-or-later
 */

#define _GNU_SOURCE

#include <arpa/inet.h>
#include <curl/curl.h>
#include <errno.h>
#include <json-c/json.h>
#include <limits.h>
#include <pthread.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#include <daemon.h>
#include <attributes/attribute_handler.h>
#include <collections/linked_list.h>
#include <networking/host.h>
#include <plugins/plugin.h>
#include <simaka_card.h>
#include <utils/chunk.h>
#include <utils/debug.h>

#define AUTS_CACHE_SIZE 4
#define RESPONSE_LIMIT (64 * 1024)

typedef struct private_eap_aka_broker_t private_eap_aka_broker_t;

typedef struct {
	bool used;
	uint8_t rand[AKA_RAND_LEN];
	uint8_t auts[AKA_AUTS_LEN];
} auts_entry_t;

typedef struct {
	char *data;
	size_t len;
} response_buffer_t;

typedef struct {
	enumerator_t public;
	bool request_ipv4_dns;
	bool request_ipv4_pcscf;
	bool request_ipv6_dns;
	bool request_ipv6_pcscf;
} attr_enumerator_t;

struct private_eap_aka_broker_t {
	plugin_t public;
	simaka_card_t card;
	attribute_handler_t attributes;
	char *url;
	char *token;
	char *rundir;
	long timeout_ms;
	pthread_mutex_t lock;
	auts_entry_t auts[AUTS_CACHE_SIZE];
	size_t next_auts;
};

#define CARD_OWNER(card) ((private_eap_aka_broker_t*)((char*)(card) - \
	offsetof(private_eap_aka_broker_t, card)))
#define ATTRIBUTE_OWNER(attributes) ((private_eap_aka_broker_t*)((char*)(attributes) - \
	offsetof(private_eap_aka_broker_t, attributes)))

static size_t collect_response(char *data, size_t size, size_t count, void *user)
{
	response_buffer_t *buffer = user;
	size_t incoming = size * count;
	char *expanded;

	if (incoming > RESPONSE_LIMIT || buffer->len > RESPONSE_LIMIT - incoming)
	{
		return 0;
	}
	expanded = realloc(buffer->data, buffer->len + incoming + 1);
	if (!expanded)
	{
		return 0;
	}
	buffer->data = expanded;
	memcpy(buffer->data + buffer->len, data, incoming);
	buffer->len += incoming;
	buffer->data[buffer->len] = '\0';
	return incoming;
}

static bool decode_field(json_object *object, const char *name, uint8_t *out,
						 size_t minimum, size_t maximum, size_t *actual)
{
	json_object *field;
	const char *encoded;
	size_t encoded_len, padded_len;
	char *padded;
	chunk_t decoded;
	bool valid = FALSE;

	if (!json_object_object_get_ex(object, name, &field) ||
		json_object_get_type(field) != json_type_string)
	{
		return FALSE;
	}
	encoded = json_object_get_string(field);
	encoded_len = json_object_get_string_len(field);
	if (!encoded || !encoded_len)
	{
		return FALSE;
	}
	padded_len = (encoded_len + 3) & ~(size_t)3;
	padded = calloc(1, padded_len + 1);
	if (!padded)
	{
		return FALSE;
	}
	memcpy(padded, encoded, encoded_len);
	memset(padded + encoded_len, '=', padded_len - encoded_len);
	decoded = chunk_from_base64(chunk_create(padded, padded_len), NULL);
	if (decoded.ptr && decoded.len >= minimum && decoded.len <= maximum)
	{
		memcpy(out, decoded.ptr, decoded.len);
		if (actual)
		{
			*actual = decoded.len;
		}
		valid = TRUE;
	}
	chunk_clear(&decoded);
	memwipe(padded, padded_len);
	free(padded);
	return valid;
}

static void strip_base64_padding(chunk_t *encoded)
{
	while (encoded->len && encoded->ptr[encoded->len - 1] == '=')
	{
		encoded->len--;
	}
}

static void cache_auts(private_eap_aka_broker_t *this,
					   const uint8_t rand[AKA_RAND_LEN],
					   const uint8_t auts[AKA_AUTS_LEN])
{
	auts_entry_t *entry;

	pthread_mutex_lock(&this->lock);
	entry = &this->auts[this->next_auts++ % AUTS_CACHE_SIZE];
	memcpy(entry->rand, rand, AKA_RAND_LEN);
	memcpy(entry->auts, auts, AKA_AUTS_LEN);
	entry->used = TRUE;
	pthread_mutex_unlock(&this->lock);
}

static status_t broker_authenticate(private_eap_aka_broker_t *this,
								uint8_t rand[AKA_RAND_LEN],
								uint8_t autn[AKA_AUTN_LEN],
								uint8_t ck[AKA_CK_LEN],
								uint8_t ik[AKA_IK_LEN],
								uint8_t res[AKA_RES_MAX], int *res_len)
{
	CURL *curl = NULL;
	struct curl_slist *headers = NULL;
	response_buffer_t response = {};
	json_object *request = NULL, *reply = NULL;
	chunk_t rand64 = chunk_empty, autn64 = chunk_empty;
	const char *body;
	long http_status = 0;
	CURLcode result;
	status_t status = FAILED;
	uint8_t auts[AKA_AUTS_LEN];
	size_t length = 0;
	char *authorization = NULL;

	rand64 = chunk_to_base64(chunk_create(rand, AKA_RAND_LEN), NULL);
	autn64 = chunk_to_base64(chunk_create(autn, AKA_AUTN_LEN), NULL);
	/* The manager API deliberately uses RFC 4648 raw/unpadded Base64.  The
	 * strongSwan helper emits canonical padding, which Go's RawStdEncoding
	 * rejects.  Keep this wire contract identical to the manager API. */
	strip_base64_padding(&rand64);
	strip_base64_padding(&autn64);
	request = json_object_new_object();
	if (!rand64.ptr || !autn64.ptr || !request)
	{
		goto cleanup;
	}
	json_object_object_add(request, "rand",
		json_object_new_string_len(rand64.ptr, rand64.len));
	json_object_object_add(request, "autn",
		json_object_new_string_len(autn64.ptr, autn64.len));
	body = json_object_to_json_string_ext(request, JSON_C_TO_STRING_PLAIN);

	curl = curl_easy_init();
	if (!curl)
	{
		goto cleanup;
	}
	headers = curl_slist_append(headers, "Content-Type: application/json");
	if (this->token && *this->token)
	{
		if (asprintf(&authorization, "Authorization: Bearer %s", this->token) < 0)
		{
			authorization = NULL;
			goto cleanup;
		}
		headers = curl_slist_append(headers, authorization);
	}
	curl_easy_setopt(curl, CURLOPT_URL, this->url);
	curl_easy_setopt(curl, CURLOPT_HTTPHEADER, headers);
	curl_easy_setopt(curl, CURLOPT_POSTFIELDS, body);
	curl_easy_setopt(curl, CURLOPT_POSTFIELDSIZE, (long)strlen(body));
	curl_easy_setopt(curl, CURLOPT_WRITEFUNCTION, collect_response);
	curl_easy_setopt(curl, CURLOPT_WRITEDATA, &response);
	curl_easy_setopt(curl, CURLOPT_CONNECTTIMEOUT_MS, this->timeout_ms);
	curl_easy_setopt(curl, CURLOPT_TIMEOUT_MS, this->timeout_ms);
	curl_easy_setopt(curl, CURLOPT_NOSIGNAL, 1L);
	curl_easy_setopt(curl, CURLOPT_PROTOCOLS_STR, "http,https");
	result = curl_easy_perform(curl);
	if (result != CURLE_OK)
	{
		DBG1(DBG_CFG, "Viti AKA broker request failed: %s",
			 curl_easy_strerror(result));
		goto cleanup;
	}
	curl_easy_getinfo(curl, CURLINFO_RESPONSE_CODE, &http_status);
	if (http_status != 200)
	{
		DBG1(DBG_CFG, "Viti AKA broker returned HTTP %ld", http_status);
		goto cleanup;
	}
	reply = json_tokener_parse(response.data ?: "");
	if (!reply || json_object_get_type(reply) != json_type_object)
	{
		DBG1(DBG_CFG, "Viti AKA broker returned invalid JSON");
		goto cleanup;
	}
	if (decode_field(reply, "auts", auts, AKA_AUTS_LEN, AKA_AUTS_LEN, NULL))
	{
		cache_auts(this, rand, auts);
		status = INVALID_STATE;
		goto cleanup;
	}
	if (!decode_field(reply, "res", res, 4, AKA_RES_MAX, &length) ||
		!decode_field(reply, "ck", ck, AKA_CK_LEN, AKA_CK_LEN, NULL) ||
		!decode_field(reply, "ik", ik, AKA_IK_LEN, AKA_IK_LEN, NULL))
	{
		DBG1(DBG_CFG, "Viti AKA broker returned invalid AKA field lengths");
		goto cleanup;
	}
	*res_len = length;
	status = SUCCESS;

cleanup:
	memwipe(auts, sizeof(auts));
	chunk_clear(&rand64);
	chunk_clear(&autn64);
	if (reply)
	{
		json_object_put(reply);
	}
	if (request)
	{
		json_object_put(request);
	}
	if (response.data)
	{
		memwipe(response.data, response.len);
		free(response.data);
	}
	if (headers)
	{
		curl_slist_free_all(headers);
	}
	if (curl)
	{
		curl_easy_cleanup(curl);
	}
	free(authorization);
	return status;
}

METHOD(simaka_card_t, get_triplet, bool,
	simaka_card_t *card, identification_t *id,
	char rand[SIM_RAND_LEN], char sres[SIM_SRES_LEN], char kc[SIM_KC_LEN])
{
	return FALSE;
}

METHOD(simaka_card_t, get_quintuplet, status_t,
	simaka_card_t *card, identification_t *id,
	char rand[AKA_RAND_LEN], char autn[AKA_AUTN_LEN], char ck[AKA_CK_LEN],
	char ik[AKA_IK_LEN], char res[AKA_RES_MAX], int *res_len)
{
	private_eap_aka_broker_t *this = CARD_OWNER(card);

	memset(ck, 0, AKA_CK_LEN);
	memset(ik, 0, AKA_IK_LEN);
	memset(res, 0, AKA_RES_MAX);
	*res_len = 0;
	return broker_authenticate(this, (uint8_t*)rand, (uint8_t*)autn,
						   (uint8_t*)ck, (uint8_t*)ik, (uint8_t*)res, res_len);
}

METHOD(simaka_card_t, resync, bool,
	simaka_card_t *card, identification_t *id,
	char rand[AKA_RAND_LEN], char auts[AKA_AUTS_LEN])
{
	private_eap_aka_broker_t *this = CARD_OWNER(card);
	bool found = FALSE;
	size_t i;

	pthread_mutex_lock(&this->lock);
	for (i = 0; i < AUTS_CACHE_SIZE; i++)
	{
		if (this->auts[i].used &&
			memeq_const(this->auts[i].rand, rand, AKA_RAND_LEN))
		{
			memcpy(auts, this->auts[i].auts, AKA_AUTS_LEN);
			memwipe(&this->auts[i], sizeof(this->auts[i]));
			found = TRUE;
			break;
		}
	}
	pthread_mutex_unlock(&this->lock);
	return found;
}

CALLBACK(is_family, bool, host_t *host, va_list args)
{
	int family;
	VA_ARGS_VGET(args, family);
	return host->get_family(host) == family;
}

static bool has_host_family(linked_list_t *hosts, int family)
{
	return hosts->find_first(hosts, is_family, NULL, family);
}

METHOD(enumerator_t, enumerate_attributes, bool,
	attr_enumerator_t *this, va_list args)
{
	configuration_attribute_type_t *type;
	chunk_t *data;

	VA_ARGS_VGET(args, type, data);
	if (this->request_ipv4_dns)
	{
		*type = INTERNAL_IP4_DNS;
		*data = chunk_empty;
		this->request_ipv4_dns = FALSE;
		return TRUE;
	}
	if (this->request_ipv4_pcscf)
	{
		*type = P_CSCF_IP4_ADDRESS;
		*data = chunk_empty;
		this->request_ipv4_pcscf = FALSE;
		return TRUE;
	}
	if (this->request_ipv6_dns)
	{
		*type = INTERNAL_IP6_DNS;
		*data = chunk_empty;
		this->request_ipv6_dns = FALSE;
		return TRUE;
	}
	if (this->request_ipv6_pcscf)
	{
		*type = P_CSCF_IP6_ADDRESS;
		*data = chunk_empty;
		this->request_ipv6_pcscf = FALSE;
		return TRUE;
	}
	return FALSE;
}

static bool persist_pcscf(private_eap_aka_broker_t *this, host_t *server)
{
	char address[INET6_ADDRSTRLEN], final[PATH_MAX], temporary[PATH_MAX];
	chunk_t raw = server->get_address(server);
	int family = server->get_family(server);
	FILE *output;
	bool written = TRUE;

	if (!inet_ntop(family, raw.ptr, address, sizeof(address)) ||
		snprintf(final, sizeof(final), "%s/pcscf", this->rundir) >= sizeof(final) ||
		snprintf(temporary, sizeof(temporary), "%s/pcscf.tmp.%ld",
				 this->rundir, (long)getpid()) >= sizeof(temporary))
	{
		return FALSE;
	}
	output = fopen(temporary, "w");
	if (!output)
	{
		return FALSE;
	}
	if (fputs(address, output) == EOF || fflush(output) != 0 ||
		fsync(fileno(output)) != 0)
	{
		written = FALSE;
	}
	if (fclose(output) != 0)
	{
		written = FALSE;
	}
	if (!written)
	{
		unlink(temporary);
		return FALSE;
	}
	if (rename(temporary, final) != 0)
	{
		unlink(temporary);
		return FALSE;
	}
	DBG1(DBG_CFG, "received and persisted P-CSCF server IP %s", address);
	return TRUE;
}

METHOD(attribute_handler_t, handle_attribute, bool,
	attribute_handler_t *attributes, ike_sa_t *ike_sa,
	configuration_attribute_type_t type, chunk_t data)
{
	private_eap_aka_broker_t *this = ATTRIBUTE_OWNER(attributes);
	host_t *server;
	int family;
	bool persisted;

	if (type == INTERNAL_IP4_DNS || type == INTERNAL_IP6_DNS)
	{
		/* Match the UE configuration request without replacing the container's
		 * LAN resolver. IMS traffic uses the returned P-CSCF directly. */
		return TRUE;
	}
	if (type != P_CSCF_IP4_ADDRESS && type != P_CSCF_IP6_ADDRESS)
	{
		return FALSE;
	}
	family = type == P_CSCF_IP4_ADDRESS ? AF_INET : AF_INET6;
	server = host_create_from_chunk(family, data, 0);
	if (!server)
	{
		return FALSE;
	}
	persisted = persist_pcscf(this, server);
	server->destroy(server);
	return persisted;
}

METHOD(attribute_handler_t, release_attribute, void,
	attribute_handler_t *attributes, ike_sa_t *ike_sa,
	configuration_attribute_type_t type, chunk_t data)
{
}

METHOD(attribute_handler_t, create_attribute_enumerator, enumerator_t*,
	attribute_handler_t *attributes, ike_sa_t *ike_sa, linked_list_t *vips)
{
	private_eap_aka_broker_t *this = ATTRIBUTE_OWNER(attributes);
	attr_enumerator_t *enumerator;

	INIT(enumerator,
		.public = {
			.enumerate = enumerator_enumerate_default,
			.venumerate = _enumerate_attributes,
			.destroy = (void*)free,
		},
		.request_ipv4_dns = has_host_family(vips, AF_INET),
		.request_ipv4_pcscf = has_host_family(vips, AF_INET),
		.request_ipv6_dns = has_host_family(vips, AF_INET6),
		.request_ipv6_pcscf = has_host_family(vips, AF_INET6),
	);
	return &enumerator->public;
}

METHOD(plugin_t, get_name, char*, private_eap_aka_broker_t *this)
{
	return "eap-aka-broker";
}

static simaka_card_t *get_card(private_eap_aka_broker_t *this)
{
	return &this->card;
}

static bool register_attributes(private_eap_aka_broker_t *this,
								plugin_feature_t *feature, bool reg, void *data)
{
	if (reg)
	{
		charon->attributes->add_handler(charon->attributes, &this->attributes);
	}
	else
	{
		charon->attributes->remove_handler(charon->attributes, &this->attributes);
	}
	return TRUE;
}

METHOD(plugin_t, get_features, int,
	private_eap_aka_broker_t *this, plugin_feature_t *features[])
{
	static plugin_feature_t provided[] = {
		PLUGIN_CALLBACK(simaka_manager_register, get_card),
			PLUGIN_PROVIDE(CUSTOM, "aka-card"),
				PLUGIN_DEPENDS(CUSTOM, "aka-manager"),
		PLUGIN_CALLBACK((plugin_feature_callback_t)register_attributes, NULL),
			PLUGIN_PROVIDE(CUSTOM, "viti-p-cscf"),
	};
	*features = provided;
	return countof(provided);
}

METHOD(plugin_t, destroy, void, private_eap_aka_broker_t *this)
{
	pthread_mutex_destroy(&this->lock);
	memwipe(this->auts, sizeof(this->auts));
	free(this->url);
	free(this->token);
	free(this->rundir);
	curl_global_cleanup();
	free(this);
}

PLUGIN_DEFINE(eap_aka_broker)
{
	private_eap_aka_broker_t *this;
	const char *url = getenv("SWU_HTTP_AKA_URL");
	const char *token = getenv("SWU_HTTP_AKA_TOKEN");
	const char *rundir = getenv("VOWIFI_RUNDIR");
	const char *timeout = getenv("SWU_HTTP_AKA_TIMEOUT");
	char *end = NULL;
	double seconds = 40.0;

	if (!url || !*url || curl_global_init(CURL_GLOBAL_DEFAULT) != CURLE_OK)
	{
		DBG1(DBG_CFG, "eap-aka-broker requires SWU_HTTP_AKA_URL");
		return NULL;
	}
	if (timeout && *timeout)
	{
		seconds = strtod(timeout, &end);
		if (!end || *end || seconds <= 0 || seconds > 300)
		{
			DBG1(DBG_CFG, "invalid SWU_HTTP_AKA_TIMEOUT");
			curl_global_cleanup();
			return NULL;
		}
	}
	INIT(this,
		.public = {
			.get_name = _get_name,
			.get_features = _get_features,
			.destroy = _destroy,
		},
		.card = {
			.get_triplet = _get_triplet,
			.get_quintuplet = _get_quintuplet,
			.resync = _resync,
			.get_pseudonym = (void*)return_null,
			.set_pseudonym = (void*)nop,
			.get_reauth = (void*)return_null,
			.set_reauth = (void*)nop,
		},
		.attributes = {
			.handle = _handle_attribute,
			.release = _release_attribute,
			.create_attribute_enumerator = _create_attribute_enumerator,
		},
		.url = strdup(url),
		.token = strdup(token ?: ""),
		.rundir = strdup(rundir ?: "/run/vowifi"),
		.timeout_ms = (long)(seconds * 1000.0),
	);
	if (!this || !this->url || !this->token || !this->rundir ||
		pthread_mutex_init(&this->lock, NULL) != 0)
	{
		if (this)
		{
			free(this->url);
			free(this->token);
			free(this->rundir);
			free(this);
		}
		curl_global_cleanup();
		return NULL;
	}
	return &this->public;
}
