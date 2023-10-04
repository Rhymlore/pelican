/***************************************************************
 *
 * Copyright (C) 2023, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

package director

// List all namespaces from origins registered at the director
func ListNamespaces() []NamespaceAd {

	serverAdMutex.RLock()
	defer serverAdMutex.RUnlock()

	serverAdItems := serverAds.Items()
	namespaces := make([]NamespaceAd, len(serverAdItems), 0)
	for _, item := range serverAdItems {
		if item.Key().Type == OriginType {
			for _, v := range item.Value() {
				namespaces = append(namespaces, v)
			}
		}
	}
	return namespaces
}